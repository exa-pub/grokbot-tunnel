// Package pathroot confines the file tools of local-exec to a root directory,
// like the desktop's local-exec root check (anchor: "Path is outside the
// allowed local-exec root and was refused"). Lexical escapes and paths whose
// realpath leaves the root are refused here; os.Root guards the access
// itself. Like the desktop's root (ult), a root given through a symlink is
// kept as it is, and followed when it is retargeted.
package pathroot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

// RootError fails a path below a root directory that cannot be opened, such
// as one removed while the tunnel runs. The desktop's realpath of the root
// then fails, the path is used as it is (_5) and opening it fails, so the
// tools report it as a failure to open Path.
type RootError struct {
	Path string // absolute, as requested
	Err  error
}

func (e *RootError) Error() string { return "open " + e.Path + ": " + e.Err.Error() }
func (e *RootError) Unwrap() error { return e.Err }

// Root is a root directory, opened again whenever its path names another
// directory: the desktop's root (ult) is a path whose realpath its check
// takes on every call (_5), so a symlink root that is retargeted, such as a
// release directory switched with ln -sfn, and a root removed and created
// again are followed. A request uses the snapshot of the root taken by its
// check.
type Root struct {
	dir string // absolute, with its symlinks kept

	mu  sync.Mutex
	cur *snapshot // nil once closed
}

// snapshot is the root directory as opened at one time. It is closed when
// it is no longer the current one and the last request using it is done.
type snapshot struct {
	fs   *os.Root
	dir  string      // Root.dir
	real string      // the realpath of fs's directory; "" if unknown
	id   os.FileInfo // of fs's directory
	refs atomic.Int32
}

// openAttempts bounds how often a snapshot is opened again because the root
// changed while it was being opened.
const openAttempts = 3

// openSnapshot opens dir and resolves its realpath, which must name the
// directory opened. If the realpath cannot be resolved, or the root keeps
// changing, it is left unknown and the realpath check is skipped, like the
// desktop's _5 when the realpath of the root fails, until a later request
// resolves it and opens the root again.
func openSnapshot(dir string) (*snapshot, error) {
	var s *snapshot
	for range openAttempts {
		if s != nil {
			s.fs.Close()
		}
		fs, err := os.OpenRoot(dir)
		if err != nil {
			return nil, err
		}
		id, err := fs.Stat(".")
		if err != nil {
			fs.Close()
			return nil, err
		}
		s = &snapshot{fs: fs, dir: dir, id: id, real: realpath(dir, id)}
		s.refs.Store(1) // held while it is the current snapshot
		if s.real != "" {
			break
		}
	}
	return s, nil
}

// realpath returns the realpath of dir if it names the directory id, or "".
func realpath(dir string, id os.FileInfo) string {
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	if st, err := os.Stat(resolved); err != nil || !os.SameFile(st, id) {
		return ""
	}
	return resolved
}

// current reports whether the root's path still names s's directory, by the
// same realpath.
func (s *snapshot) current() bool {
	resolved, err := filepath.EvalSymlinks(s.dir)
	switch {
	case err != nil:
		resolved = s.dir
	case resolved != s.real:
		return false
	}
	st, err := os.Stat(resolved)
	return err == nil && os.SameFile(st, s.id)
}

func (s *snapshot) release() {
	if s.refs.Add(-1) == 0 {
		s.fs.Close()
	}
}

// Open opens dir as the root. Like the desktop, symlinks in dir itself are
// kept: a path is checked lexically against dir, and only its realpath
// against dir's.
func Open(dir string) (*Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	s, err := openSnapshot(abs)
	if err != nil {
		return nil, err
	}
	return &Root{dir: abs, cur: s}, nil
}

// Dir returns the absolute root directory, with its symlinks kept.
func (r *Root) Dir() string { return r.dir }

// Abs returns the absolute path of rel.
func (r *Root) Abs(rel string) string { return filepath.Join(r.dir, rel) }

// Close releases the root directory handle once no request uses it.
func (r *Root) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil {
		r.cur.release()
		r.cur = nil
	}
	return nil
}

// MkdirAll creates the directory rel below the root, and its parents.
func (r *Root) MkdirAll(rel string, perm os.FileMode) error {
	s, err := r.acquire()
	if err != nil {
		return err
	}
	defer s.release()
	return s.fs.MkdirAll(rel, perm)
}

// OpenFile opens the file rel below the root.
func (r *Root) OpenFile(rel string, flag int, perm os.FileMode) (*os.File, error) {
	s, err := r.acquire()
	if err != nil {
		return nil, err
	}
	defer s.release()
	return s.fs.OpenFile(rel, flag, perm)
}

// Remove removes the file rel below the root.
func (r *Root) Remove(rel string) error {
	s, err := r.acquire()
	if err != nil {
		return err
	}
	defer s.release()
	return s.fs.Remove(rel)
}

// Relative converts path, absolute, relative to the root, starting with "~"
// or a file:// URL, to a path relative to the root. It rejects lexical
// escapes; os.Root rejects symlink escapes.
//
// Like the desktop's root check (_5, since 1.0.1), "~" and file:// URLs are
// expanded (Ir) and the expanded path is checked, for reads, listings,
// uploads and downloads alike.
func (r *Root) Relative(path string) (string, error) {
	rel, ok := r.inside(path)
	if !ok {
		return "", &OutsideError{Path: path}
	}
	return rel, nil
}

// Path is a path checked against a snapshot of the root, which it holds
// until Close.
type Path struct {
	// Rel is the path relative to Dir, as requested: for messages and
	// results.
	Rel string
	// Real is the same path relative to the root's realpath, with the
	// symlinks of its existing part resolved: the name to use with FS.
	// os.Root refuses absolute symlinks and those that leave the root on
	// the way, even if their target is inside it, which the desktop
	// follows; Real has none left, while os.Root still guards the access
	// against a symlink swapped in after the check.
	Real string
	snap *snapshot
}

// FS returns the root directory as opened when the path was checked, for
// operations on Real that must not leave it.
func (p *Path) FS() *os.Root { return p.snap.fs }

// Close releases the snapshot of the root.
func (p *Path) Close() { p.snap.release() }

// Check converts path like Relative and also refuses a path whose existing
// part leads outside the root through a symlink, like the desktop's check
// of file paths (_5): the realpath of the path against the realpath of the
// root, both taken now. A root directory that cannot be opened fails with a
// *RootError. The caller closes the path.
func (r *Root) Check(path string) (*Path, error) {
	rel, err := r.Relative(path)
	if err != nil {
		return nil, err
	}
	s, err := r.acquire()
	switch {
	case errors.Is(err, os.ErrClosed):
		return nil, err
	case err != nil:
		var errno syscall.Errno
		if errors.As(err, &errno) {
			err = errno
		}
		return nil, &RootError{Path: r.Abs(rel), Err: err}
	}
	p := &Path{Rel: rel, Real: rel, snap: s}
	if s.real == "" {
		return p, nil
	}
	// s.real has no symlinks, so the path resolves from it as from the
	// root's path, and in the directory that s.fs has open.
	resolved, ok := within(s.real, resolveExisting(filepath.Join(s.real, rel)))
	if !ok || danglingOut(s.real, filepath.Join(s.real, rel)) {
		s.release()
		return nil, &OutsideError{Path: path, Symlink: true}
	}
	p.Real = resolved
	return p, nil
}

// resolveExisting resolves the symlinks of the longest existing prefix of
// path and appends the rest.
func resolveExisting(path string) string {
	prefix, rest := splitExisting(path)
	return filepath.Join(prefix, rest)
}

// splitExisting returns the longest prefix of path whose symlinks resolve,
// resolved, and the rest of path.
func splitExisting(path string) (prefix, rest string) {
	for {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return resolved, rest
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path, rest
		}
		rest = filepath.Join(filepath.Base(path), rest)
		path = parent
	}
}

// danglingOut reports whether path goes through a symlink that does not
// resolve and points outside root: a directory of the path, not its last
// name, which an upload replaces like the desktop. os.Root refuses to go
// through such a symlink too, but in words that depend on the Go release:
// creating a directory below it fails with "path escapes from parent" in
// newer releases and with EEXIST in Go 1.25. Refusing it here answers the
// same with every toolchain.
func danglingOut(root, path string) bool {
	prefix, rest := splitExisting(path)
	first, below, _ := strings.Cut(rest, string(filepath.Separator))
	if below == "" {
		return false
	}
	link := filepath.Join(prefix, first)
	target, err := os.Readlink(link)
	if err != nil {
		return false // not a symlink, or gone
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(prefix, target)
	}
	_, inside := within(root, resolveExisting(filepath.Clean(target)))
	return !inside
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

// OpenRegular opens p, a regular file of at most MaxFile bytes. O_NONBLOCK
// keeps a FIFO from blocking until a writer appears; it does not affect
// regular files.
func (p *Path) OpenRegular() (*os.File, error) {
	f, err := p.FS().OpenFile(p.Real, os.O_RDONLY|syscall.O_NONBLOCK, 0)
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

// UploadTarget resolves the symlinks in p, so that an upload writes through
// a symlink to its target instead of replacing it; the target must stay
// inside the root. The target is returned relative to the root's realpath,
// for FS. Like the desktop's atomic write (cp), a path that does not
// resolve, such as a dangling symlink, is used as it is: the upload replaces
// the dangling symlink with a regular file.
func (p *Path) UploadTarget() (string, error) {
	base := p.snap.real
	if base == "" {
		base = p.snap.dir
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(base, p.Real))
	if errors.Is(err, os.ErrNotExist) {
		return p.Real, nil
	}
	if err != nil {
		return "", err
	}
	// The root's own symlinks are resolved too, so resolved is below the
	// root's realpath.
	target, ok := within(base, resolved)
	if !ok {
		return "", &OutsideError{Path: resolved}
	}
	return target, nil
}

// acquire returns the snapshot of the directory that the root's path names
// now, opening it if that is another directory than before; the caller
// releases it. Requests run in parallel: the check that the snapshot is
// current runs outside the lock, opening one inside it.
func (r *Root) acquire() (*snapshot, error) {
	r.mu.Lock()
	cur := r.cur
	if cur == nil {
		r.mu.Unlock()
		return nil, os.ErrClosed
	}
	cur.refs.Add(1)
	r.mu.Unlock()
	if cur.current() {
		return cur, nil
	}
	cur.release()

	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.cur == nil:
		return nil, os.ErrClosed
	case r.cur != cur && r.cur.current(): // opened by another request
		r.cur.refs.Add(1)
		return r.cur, nil
	}
	if st, err := os.Stat(r.dir); err != nil {
		return nil, err
	} else if !st.IsDir() {
		return nil, &os.PathError{Op: "open", Path: r.dir, Err: syscall.ENOTDIR}
	}
	s, err := openSnapshot(r.dir)
	if err != nil {
		return nil, err
	}
	r.cur.release()
	r.cur = s
	s.refs.Add(1)
	return s, nil
}

func (r *Root) inside(path string) (string, bool) {
	if path == "" {
		path = "."
	}
	path = expandHome(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.dir, path)
	}
	return within(r.dir, path)
}

// within returns the absolute path relative to dir if it is lexically below
// dir.
func within(dir, path string) (string, bool) {
	rel, err := filepath.Rel(dir, filepath.Clean(path))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}
