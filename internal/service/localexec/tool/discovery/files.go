package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"syscall"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
)

// absent reports errors that mean "not there", like the desktop does for
// ENOENT, ENOTDIR and ELOOP.
func absent(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || errors.Is(err, syscall.ELOOP)
}

func statPath(path string) (os.FileInfo, error) {
	return os.Stat(path) //nolint:gosec // see above
}

// exists reports whether path is a file (wantDir false) or a directory,
// following symlinks.
func exists(path string, wantDir bool) (bool, error) {
	st, err := statPath(path)
	if absent(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if wantDir {
		return st.IsDir(), nil
	}
	return st.Mode().IsRegular(), nil
}

// entryIs reports whether a directory entry is a file or directory itself or
// through a symlink.
func entryIs(path string, entry os.DirEntry, wantDir bool) (bool, error) {
	switch {
	case wantDir && entry.IsDir(), !wantDir && entry.Type().IsRegular():
		return true, nil
	case entry.Type()&os.ModeSymlink != 0:
		return exists(path, wantDir)
	}
	return false, nil
}

// openDiscovery opens a regular file. O_NONBLOCK keeps a FIFO from blocking
// until a writer appears.
func openDiscovery(ctx context.Context, path string) (*os.File, os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // see above
	if err != nil {
		return nil, nil, err
	}
	st, err := f.Stat()
	if err == nil && !st.Mode().IsRegular() {
		err = errors.New("expected regular file: " + path)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, st, nil
}

// readConfig reads a whole configuration file; a missing file reads as
// empty. maxConfigBytes only guards against absurd files.
func readConfig(ctx context.Context, path string) ([]byte, error) {
	f, _, err := openDiscovery(ctx, path)
	if absent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err == nil && len(b) > maxConfigBytes {
		err = fmt.Errorf("AI tool configuration exceeds %d bytes", maxConfigBytes)
	}
	return b, err
}

// readTail reads the last historyBytes of a file, dropping the partial first
// line if the file was cut; a missing file reads as empty.
func readTail(ctx context.Context, path string) ([]byte, error) {
	f, st, err := openDiscovery(ctx, path)
	if absent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	n := min(st.Size(), historyBytes)
	b, err := proc.ReadAtMost(f, n, st.Size()-n)
	if err != nil {
		return nil, err
	}
	if n < st.Size() {
		if _, rest, found := bytes.Cut(b, []byte("\n")); found {
			b = rest
		} else {
			b = nil
		}
	}
	return b, nil
}

// discoveryEntries lists at most maxDirEntries entries of a directory sorted
// by name; a missing directory is empty.
func discoveryEntries(ctx context.Context, path string, descending bool) ([]os.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if absent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if descending {
		slices.Reverse(entries)
	}
	return entries[:min(len(entries), maxDirEntries)], nil
}
