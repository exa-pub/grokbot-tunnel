package pathroot

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"
)

// cwdProbeTimeout bounds the stat of CwdState, like the desktop's (Iyr).
const cwdProbeTimeout = time.Second

// Cwd resolves a shell working directory like the desktop: "~" and file://
// URLs are expanded, a relative path is taken from the root, and any
// existing directory is accepted, inside the root or not; the shell is not
// confined. Otherwise the command runs in the root, with a warning for the
// agent. An empty path is the root.
func (r *Root) Cwd(path string) (dir, warning string) {
	p := r.expand(path)
	if p == "" {
		return r.dir, ""
	}
	if isDir(p) {
		return p, ""
	}
	return r.dir, r.missingCwd(path)
}

// ResolveCwd resolves the working directory a shell request asks for, like
// the desktop's dispatcher before it runs the command (ilt): a blank path
// becomes "", the session's cwd; an existing directory, expanded like Cwd,
// is used; a missing one or a file falls back to the root, with the warning
// a foreground command reports. Any other failure to check the path is
// returned in the words of Node's fs, as the desktop rethrows it.
func (r *Root) ResolveCwd(path string) (dir, warning string, err error) {
	p := r.expand(path)
	if p == "" {
		return "", "", nil
	}
	ok, err := statDir(p)
	switch {
	case err != nil:
		return "", "", err
	case ok:
		return p, "", nil
	}
	return r.dir, r.missingCwd(path), nil
}

func (r *Root) missingCwd(path string) string {
	return fmt.Sprintf("working directory %s does not exist on this machine; running in %s instead\n", path, r.dir)
}

// IsDir reports whether dir, expanded like Cwd, is an existing directory.
func (r *Root) IsDir(dir string) bool {
	p := r.expand(dir)
	return p != "" && isDir(p)
}

// CwdState describes a shell working directory for a failed request, like
// the desktop (flt): "exists" or "missing"; nil if it cannot be determined
// within a second.
func (r *Root) CwdState(path string) *string {
	p := r.expand(path)
	if p == "" {
		p = r.dir
	}
	probe := make(chan *string, 1) // a stat that hangs leaves its goroutine behind
	go func() {
		ok, err := statDir(p)
		switch {
		case err != nil:
			probe <- nil
		case ok:
			probe <- proto.String("exists")
		default:
			probe <- proto.String("missing")
		}
	}()
	timer := time.NewTimer(cwdProbeTimeout)
	defer timer.Stop()
	select {
	case state := <-probe:
		return state
	case <-timer.C:
		return nil
	}
}

// statDir reports whether path is a directory, like the desktop's llt: a
// path that does not exist (ENOENT, ENOTDIR) is not one, and any other
// failure is returned in the words of Node's fs.
func statDir(path string) (bool, error) {
	st, err := os.Stat(path)
	switch {
	case err == nil:
		return st.IsDir(), nil
	case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ENOTDIR):
		return false, nil
	}
	return false, nodeFSError("stat", path, err)
}

// uvTexts are libuv's descriptions of the errors a stat can fail with, which
// Node's fs errors carry; others fall back to the C library's.
var uvTexts = map[syscall.Errno]string{
	syscall.EACCES:       "permission denied",
	syscall.EPERM:        "operation not permitted",
	syscall.ELOOP:        "too many symbolic links encountered",
	syscall.ENAMETOOLONG: "name too long",
	syscall.EIO:          "i/o error",
	syscall.EOVERFLOW:    "value too large for defined data type",
	syscall.ENOMEM:       "not enough memory",
}

// nodeFSError renders err of the file system call op on path like Node's fs:
// "EACCES: permission denied, stat '/x'".
func nodeFSError(op, path string, err error) error {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return err
	}
	name := unix.ErrnoName(errno)
	if name == "" {
		return err
	}
	text, ok := uvTexts[errno]
	if !ok {
		text = errno.Error()
	}
	return &nodeError{errno: errno, text: fmt.Sprintf("%s: %s, %s '%s'", name, text, op, path)}
}

// nodeError is an errno in the words of Node's fs.
type nodeError struct {
	errno syscall.Errno
	text  string
}

func (e *nodeError) Error() string { return e.text }
func (e *nodeError) Unwrap() error { return e.errno }

// expand trims path, expands "~" and file:// URLs and makes a relative path
// absolute below the root; empty stays empty.
func (r *Root) expand(path string) string {
	p := strings.TrimSpace(path)
	if p == "" {
		return ""
	}
	p = expandHome(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.dir, p)
	}
	return filepath.Clean(p)
}

// expandHome expands the forms of a path that the desktop resolves before
// using it (cr: kOe, FNt): "~" and "~/…" become the home directory, and a
// file:// URL its path; a URL that names a host, which Node's fileURLToPath
// refuses, and any other path stay as they are.
func expandHome(p string) string {
	switch {
	case p == "~" || strings.HasPrefix(p, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	case strings.HasPrefix(p, "file://"):
		if u, err := url.Parse(p); err == nil && u.Host == "" && u.Path != "" {
			return u.Path
		}
	}
	return p
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
