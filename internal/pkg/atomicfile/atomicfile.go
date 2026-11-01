// Package atomicfile replaces files so that readers see either the old or the
// complete new content: the data goes to a temporary file in the same
// directory, which is synced and renamed over the target.
package atomicfile

import (
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// Options adjust Write. The zero value writes a file readable only by the
// owner.
type Options struct {
	// Mode is the file's permission bits; 0 means 0600 unless ExactMode is
	// set.
	Mode os.FileMode
	// ExactMode uses Mode even when it is 0, as for a copy of a file's mode.
	ExactMode bool
	// Umask applies the process umask to Mode, as for a newly created file.
	// Otherwise the file gets Mode exactly.
	Umask bool
	// Owner, if set, is given to the file before it replaces the target, so
	// that a root process keeps a user's file theirs. A refusal (EPERM) is
	// ignored.
	Owner *Owner
	// Prefix names the temporary file; the default is "."+base+".tmp-".
	Prefix string
	// Check runs on the complete temporary file before it replaces the
	// target; an error leaves the target untouched. It gets the temporary
	// file's path in the same form as the target's.
	Check func(tmp string) error
	// SyncDir also syncs the directory, so that the rename survives a crash.
	SyncDir bool
}

// Owner is a file's user and group.
type Owner struct{ UID, GID int }

// fsys is the part of a file system Write needs: a directory or an os.Root.
type fsys interface {
	OpenFile(name string, flag int, perm os.FileMode) (*os.File, error)
	Rename(oldname, newname string) error
	Remove(name string) error
}

type osFS struct{}

func (osFS) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return os.OpenFile(name, flag, perm) //nolint:gosec // a path the caller chose
}
func (osFS) Rename(oldname, newname string) error { return os.Rename(oldname, newname) }
func (osFS) Remove(name string) error             { return os.Remove(name) }

// Write atomically replaces path with data. A failure leaves path untouched
// and removes the temporary file.
func Write(path string, data []byte, opts Options) error {
	return write(osFS{}, path, data, opts)
}

// WriteIn is Write for a path relative to root, which it never leaves.
func WriteIn(root *os.Root, rel string, data []byte, opts Options) error {
	return write(root, rel, data, opts)
}

func write(fs fsys, path string, data []byte, opts Options) error {
	mode := opts.Mode
	if mode == 0 && !opts.ExactMode {
		mode = 0o600
	}
	f, tmp, err := createTemp(fs, path, opts.Prefix, mode)
	if err != nil {
		return err
	}
	defer fs.Remove(tmp) //nolint:errcheck // gone after the rename
	err = fill(f, data, mode, opts)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && opts.Check != nil {
		err = opts.Check(tmp)
	}
	if err != nil {
		return err
	}
	if err = fs.Rename(tmp, path); err != nil || !opts.SyncDir {
		return err
	}
	return syncDir(fs, filepath.Dir(path))
}

// fill writes data to the temporary file and syncs it. The owner and mode
// are set after writing: a write, and a change of owner, clear setuid and
// setgid.
func fill(f *os.File, data []byte, mode os.FileMode, opts Options) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	if opts.Owner != nil {
		if err := f.Chown(opts.Owner.UID, opts.Owner.GID); err != nil && !errors.Is(err, syscall.EPERM) {
			return err
		}
	}
	if !opts.Umask {
		if err := f.Chmod(mode); err != nil {
			return err
		}
	}
	return f.Sync()
}

// createTemp creates a new file next to path, like os.CreateTemp.
func createTemp(fs fsys, path, prefix string, mode os.FileMode) (*os.File, string, error) {
	if prefix == "" {
		prefix = "." + filepath.Base(path) + ".tmp-"
	}
	dir := filepath.Dir(path)
	for range 10000 {
		name := filepath.Join(dir, prefix+strconv.FormatUint(rand.Uint64(), 36)) //nolint:gosec // a unique name, not a secret
		// Special bits go through Chmod; os.Root refuses them here.
		f, err := fs.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode.Perm())
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return f, name, err
	}
	return nil, "", os.ErrExist
}

func syncDir(fs fsys, dir string) error {
	d, err := fs.OpenFile(dir, os.O_RDONLY, 0)
	if err != nil {
		return err
	}
	err = d.Sync()
	if closeErr := d.Close(); err == nil {
		err = closeErr
	}
	return err
}
