package shellsession

import (
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
)

const (
	// tempPrefix starts the name of a session's private directory in the
	// system's temporary directory: grok-shell-<pid>-<random>. The pid only
	// helps to tell whose directory it is; the lock tells whether it is used.
	tempPrefix = "grok-shell-"
	// legacyPrefix starts the fallback snapshots that versions without the
	// private directory left in the temporary directory.
	legacyPrefix = "grok-shell-state-"
	// createAttempts bounds the directories created in a row that another
	// process removed before they were locked.
	createAttempts = 3
	// staleAge is the age below which an unlocked private directory is kept:
	// a new one is locked right after its creation, and removeStale must
	// not take it in between.
	staleAge = time.Minute
)

// tempDir is a session's private directory for the shells' temporary files,
// mode 0700, in the system's temporary directory. The desktop has none: it
// passes the state through descriptors and creates a temporary directory per
// command only on Windows.
//
// The session holds a shared flock on the directory while it lives, through
// a descriptor that shells do not inherit. systemd-tmpfiles skips a locked
// directory and everything below it (tmpfiles.d(5)), so an idle session does
// not lose it to the age limit of /tmp, and removeStale of other processes
// leaves it alone, even in another pid namespace that shares /tmp. A
// directory that disappears anyway is created again by file.
type tempDir struct {
	mu   sync.Mutex // guards the fields below
	path string
	// lock is the directory, open with a shared flock.
	lock   *os.File
	closed bool
}

// newTempDir creates the private directory of a session, after removing the
// ones that no process uses any more.
func newTempDir() (*tempDir, error) {
	tmp := os.TempDir()
	removeStale(tmp)
	d := &tempDir{}
	if err := d.create(tmp); err != nil {
		return nil, err
	}
	return d, nil
}

// create makes a new private directory in tmp, locks it and makes it the
// current one. removeStale keeps a directory younger than staleAge, so it
// does not remove one between its creation and the lock; should it happen
// anyway, say after the clock jumped, the lock is refused or the directory is
// gone from its path once locked, and a new one is created.
func (d *tempDir) create(tmp string) error {
	for range createAttempts {
		path, err := os.MkdirTemp(tmp, tempPrefix+strconv.Itoa(os.Getpid())+"-")
		if err != nil {
			return fmt.Errorf("create the shell's temporary directory in %s (set TMPDIR to a writable directory): %w", tmp, err)
		}
		lock, err := lockDir(path, syscall.LOCK_SH|syscall.LOCK_NB)
		switch {
		case err == nil && sameDir(path, lock):
			d.path, d.lock = path, lock
			return nil
		case err == nil:
			proc.CloseLogged(lock)
		case !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.EWOULDBLOCK):
			return fmt.Errorf("lock the shell's temporary directory %s: %w", path, err)
		}
		log.Printf("shell temporary directory %s was removed before it was locked; creating another", path)
	}
	return fmt.Errorf("create the shell's temporary directory in %s: removed by another process %d times", tmp, createAttempts)
}

// file creates a file in the directory with create. A directory that is gone
// is created again, once, for this and later files; files of running
// commands in the old one keep working through their descriptors.
func (d *tempDir) file(create func(dir string) (*os.File, error)) (*os.File, error) {
	d.mu.Lock()
	dir := d.path
	d.mu.Unlock()
	f, err := create(dir)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		return f, err
	}
	if dir, err = d.renew(dir, err); err != nil {
		return nil, err
	}
	return create(dir)
}

// renew replaces the directory old, which a file could not be created in
// because of cause, by a new one, unless another command already did, the
// session is closed or old is still in place.
func (d *tempDir) renew(old string, cause error) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case d.closed:
		return "", cause
	case d.path != old:
		return d.path, nil
	case sameDir(d.path, d.lock):
		return "", cause
	}
	log.Printf("shell temporary directory %s is gone; creating another", old)
	proc.CloseLogged(d.lock)
	d.lock = nil
	if err := d.create(os.TempDir()); err != nil {
		return "", err
	}
	return d.path, nil
}

// close removes the directory and releases its lock.
func (d *tempDir) close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	err := os.RemoveAll(d.path)
	if d.lock != nil {
		err = errors.Join(err, d.lock.Close())
	}
	return err
}

// lockDir opens the directory at path, not followed if a symlink, without
// passing it to children, and flocks it with how.
func lockDir(path string, how int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	if err := flock(f, how); err != nil {
		proc.CloseLogged(f)
		return nil, err
	}
	return f, nil
}

// flock applies flock(2) to f, retrying on EINTR.
func flock(f *os.File, how int) error {
	conn, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var lockErr error
	if err := conn.Control(func(fd uintptr) {
		for {
			if lockErr = syscall.Flock(int(fd), how); !errors.Is(lockErr, syscall.EINTR) {
				return
			}
		}
	}); err != nil {
		return err
	}
	return lockErr
}

// sameDir reports whether path still names the directory open as f.
func sameDir(path string, f *os.File) bool {
	open, err := f.Stat()
	if err != nil {
		return false
	}
	info, err := os.Lstat(path)
	return err == nil && os.SameFile(open, info)
}

// removeStale removes the user's private directories in tmp that no session
// holds locked, and the user's fallback snapshots of older versions that no
// process has open. A process killed before Close leaves its directory
// behind, and the EXIT trap of a shell that outlives it writes the whole
// environment there.
func removeStale(tmp string) {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		log.Printf("shell temporary files: %v", err)
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(tmp, name)
		switch {
		case entry.Type().IsRegular() && strings.HasPrefix(name, legacyPrefix):
			if info, err := os.Lstat(path); err == nil && ownFile(info) {
				removeUnused(path)
			}
		case entry.IsDir() && privateDirName(name):
			removeUnlocked(path)
		}
	}
}

// removeUnused removes the file at path unless a process has it open, as an
// older version keeps its fallback snapshot open while the command runs: the
// kernel grants a write lease only on a file that nobody else has open. A
// file whose lease cannot be taken for any reason is kept.
func removeUnused(path string) {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	defer f.Close()
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_SETLEASE, syscall.F_WRLCK); errno != 0 {
		return
	}
	proc.RemoveLogged(path)
}

// removeUnlocked removes the user's private directory at path unless a
// session holds it locked: an exclusive flock is granted only on a directory
// that no session of any process, in any pid namespace, has locked, unlike a
// signal to the pid in its name. A directory younger than staleAge, which its
// session may be about to lock, or that cannot be locked for any reason is
// kept.
func removeUnlocked(path string) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !ownFile(info) || time.Since(info.ModTime()) < staleAge {
		return
	}
	if flock(f, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return
	}
	if err := os.RemoveAll(path); err != nil {
		log.Printf("remove stale shell files: %v", err)
	}
}

// privateDirName reports whether name is that of a private directory,
// grok-shell-<pid>-<random>.
func privateDirName(name string) bool {
	rest, ok := strings.CutPrefix(name, tempPrefix)
	if !ok {
		return false
	}
	pidText, _, ok := strings.Cut(rest, "-")
	if !ok {
		return false
	}
	pid, err := strconv.Atoi(pidText)
	return err == nil && pid > 0
}

// ownFile reports whether info describes a file of the current user.
func ownFile(info fs.FileInfo) bool {
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(st.Uid) == int64(os.Getuid())
}
