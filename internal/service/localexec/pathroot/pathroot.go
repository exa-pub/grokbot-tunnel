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

// ErrOutside matches every *OutsideError.
var ErrOutside = errors.New("path is outside the allowed local-exec root")

// OutsideError refuses a path outside the root, in the desktop's words
// (SandLocalExecPathError).
type OutsideError struct {
	Path    string // as requested
	Symlink bool   // the path leads outside only through a symlink
}

func (e *OutsideError) Error() string {
	if e.Symlink {
		return "Path resolves through a symlink to outside the allowed local-exec root and was refused: " + e.Path
	}
	return "Path is outside the allowed local-exec root and was refused: " + e.Path
}

// Is makes errors.Is(err, ErrOutside) match.
func (e *OutsideError) Is(target error) bool { return target == ErrOutside }

// StackTrace is the first line of the desktop's stack trace for the error;
// its frames are those of JavaScript and have no counterpart here.
func (e *OutsideError) StackTrace() string { return "SandLocalExecPathError: " + e.Error() }

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
	rel, ok := r.inside(path)
	if !ok {
		return "", &OutsideError{Path: path}
	}
	return rel, nil
}

// Check is Relative that also refuses a path whose existing part leads
// outside the root through a symlink, like the desktop's check of file
// paths. os.Root still guards the access itself.
func (r *Root) Check(path string) (string, error) {
	rel, err := r.Relative(path)
	if err != nil {
		return "", err
	}
	if _, ok := r.inside(resolveExisting(filepath.Join(r.dir, rel))); !ok {
		return "", &OutsideError{Path: path, Symlink: true}
	}
	return rel, nil
}

// resolveExisting resolves the symlinks of the longest existing prefix of
// path and appends the rest.
func resolveExisting(path string) string {
	rest := ""
	for {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Join(path, rest)
		}
		rest = filepath.Join(filepath.Base(path), rest)
		path = parent
	}
}

// Escaped reports whether os.Root refused a path leading outside the root.
// Such requests fail instead of producing a typed result.
func Escaped(err error) bool { return err != nil && strings.Contains(err.Error(), "path escapes") }

// Refuse turns an error of os.Root refusing path because it leads outside
// the root (Escaped), such as through a dangling or swapped symlink that
// Check could not follow, into the desktop's refusal of a symlink escape;
// other errors are returned as they are. The desktop lets such a path
// through; refusing it is stricter on purpose (rnd-v2/SUMMARY.md).
func Refuse(path string, err error) error {
	if Escaped(err) {
		return &OutsideError{Path: path, Symlink: true}
	}
	return err
}

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
	rel, err := r.Check(path)
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

// UploadTarget resolves the symlinks in rel, so that an upload writes
// through a symlink to its target instead of replacing it; the target must
// stay inside the root. Like the desktop's atomic write (f0), a path that
// does not resolve, such as a dangling symlink, is used as it is: the upload
// replaces the dangling symlink with a regular file.
func (r *Root) UploadTarget(rel string) (string, error) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(r.dir, rel))
	if errors.Is(err, os.ErrNotExist) {
		return rel, nil
	}
	if err != nil {
		return "", err
	}
	return r.Relative(resolved)
}

func (r *Root) inside(path string) (string, bool) {
	if path == "" {
		path = "."
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.dir, path)
	}
	rel, err := filepath.Rel(r.dir, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}
