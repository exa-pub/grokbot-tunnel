// Package atomicfile replaces files so that readers see either the old or the
// complete new content: the data goes to a temporary file in the same
// directory, which is synced and renamed over the target.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Options adjust Write. The zero value writes a file readable only by the
// owner.
type Options struct {
	// Mode is the file's permission bits, set regardless of the umask; 0
	// keeps 0600.
	Mode os.FileMode
	// Prefix names the temporary file; the default is "."+base+".tmp-".
	Prefix string
	// Check runs on the complete temporary file before it replaces path; an
	// error leaves path untouched.
	Check func(tmp string) error
	// SyncDir also syncs the directory, so that the rename survives a crash.
	SyncDir bool
}

// Write atomically replaces path with data. A failure leaves path untouched
// and removes the temporary file.
func Write(path string, data []byte, opts Options) error {
	dir := filepath.Dir(path)
	prefix := opts.Prefix
	if prefix == "" {
		prefix = "." + filepath.Base(path) + ".tmp-"
	}
	f, err := os.CreateTemp(dir, prefix)
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(data); err == nil && opts.Mode != 0 {
		err = f.Chmod(opts.Mode)
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil && opts.Check != nil {
		err = opts.Check(tmp)
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil || !opts.SyncDir {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // the directory of a path the caller chose
	if err != nil {
		return err
	}
	err = d.Sync()
	if closeErr := d.Close(); err == nil {
		err = closeErr
	}
	return err
}
