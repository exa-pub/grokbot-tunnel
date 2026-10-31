// Package pathroot confines the file tools of local-exec to a root directory,
// like the desktop's local-exec root check (anchor: "Path is outside the
// allowed local-exec root and was refused"). Lexical escapes are refused here;
// symlink escapes are refused by os.Root.
package pathroot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// MaxFile is the largest file read or transferred.
const MaxFile = 100 << 20

// maxSymlinks bounds a chain of symlinks, like Linux's MAXSYMLINKS.
const maxSymlinks = 40

// ErrOutside refuses a path that leads outside the root lexically.
var ErrOutside = errors.New("path is outside the allowed local-exec root")

// Root is an open root directory.
type Root struct {
	dir string
	fs  *os.Root
}

// Open opens dir as the root, resolving symlinks in dir itself.
func Open(dir string) (*Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	fs, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	return &Root{dir: abs, fs: fs}, nil
}

// Dir returns the absolute root directory.
func (r *Root) Dir() string { return r.dir }

// FS returns the root for operations that must not leave it.
func (r *Root) FS() *os.Root { return r.fs }

// Abs returns the absolute path of rel.
func (r *Root) Abs(rel string) string { return filepath.Join(r.dir, rel) }

// Close releases the root directory handle.
func (r *Root) Close() error { return r.fs.Close() }

// Relative converts path, absolute or relative to the root, to a path
// relative to the root. It rejects lexical escapes; os.Root rejects symlink
// escapes.
func (r *Root) Relative(path string) (string, error) {
	if path == "" {
		path = "."
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.dir, path)
	}
	rel, err := filepath.Rel(r.dir, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrOutside
	}
	return rel, nil
}

// Escaped reports whether os.Root refused a path leading outside the root.
// Such requests fail instead of producing a typed result.
func Escaped(err error) bool { return err != nil && strings.Contains(err.Error(), "path escapes") }

// FileTooLarge refuses a file of size bytes in the desktop's words.
func FileTooLarge(size int64) error {
	mib := func(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }
	//nolint:staticcheck // desktop text
	return fmt.Errorf("File is %s, which exceeds Grok Bot's %s limit for reading or transferring a single file over local-exec. "+
		"Read a slice with offset/limit, or use a shell command (grep, head, tail) to extract just what you need.", mib(size), mib(MaxFile))
}

// OpenRegular opens a regular file of at most MaxFile bytes below the root.
// O_NONBLOCK keeps a FIFO from blocking until a writer appears; it does not
// affect regular files.
func (r *Root) OpenRegular(path string) (*os.File, error) {
	rel, err := r.Relative(path)
	if err != nil {
		return nil, err
	}
	f, err := r.fs.OpenFile(rel, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	switch {
	case err != nil:
	case !st.Mode().IsRegular():
		err = errors.New("expected regular file")
	case st.Size() > MaxFile:
		err = FileTooLarge(st.Size())
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// UploadTarget follows symlinks in rel, including dangling ones, so that an
// upload writes through a symlink to its target instead of replacing it. The
// target must stay inside the root.
func (r *Root) UploadTarget(rel string) (string, error) {
	for range maxSymlinks {
		path := filepath.Join(r.dir, rel)
		resolved, err := filepath.EvalSymlinks(path)
		if err == nil {
			return r.Relative(resolved)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		// Something is missing: either the file, which is created, or the
		// target of a dangling symlink, which is followed.
		target, err := os.Readlink(path)
		if err != nil {
			return rel, nil //nolint:nilerr // not a symlink: create the file
		}
		if !filepath.IsAbs(target) {
			dir, err := filepath.EvalSymlinks(filepath.Dir(path))
			if err != nil {
				return "", err
			}
			target = filepath.Join(dir, target)
		}
		if rel, err = r.Relative(target); err != nil {
			return "", err
		}
	}
	return "", syscall.ELOOP
}
