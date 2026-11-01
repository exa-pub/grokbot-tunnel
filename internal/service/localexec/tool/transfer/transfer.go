// Package transfer moves whole files between the server and the root, like
// the desktop's upload and download handlers of the user-computer channel:
// an upload replaces its target atomically, a download streams 1 MiB chunks.
package transfer

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/atomicfile"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

// Chunk is the size of a download chunk.
const Chunk = 1 << 20

// Transfer uploads and downloads files below a root.
type Transfer struct {
	root *pathroot.Root
}

// New returns a Transfer for files below root.
func New(root *pathroot.Root) *Transfer { return &Transfer{root: root} }

// Upload writes data to path, following symlinks inside the root. An existing
// file keeps its permissions; a new one gets 0644 less the umask. Readers
// never see partial data.
func (t *Transfer) Upload(path string, data []byte) error {
	if len(data) > pathroot.MaxFile {
		return pathroot.FileTooLarge(int64(len(data)))
	}
	rel, err := t.root.Check(path)
	if err != nil {
		return err
	}
	if rel, err = t.root.UploadTarget(rel); errors.Is(err, pathroot.ErrOutside) {
		return &pathroot.OutsideError{Path: path, Symlink: true}
	} else if err != nil {
		return err
	}
	if err = t.root.FS().MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	mode, links, existed, err := t.mode(rel)
	if err != nil {
		return err
	}
	if links > 1 {
		// Replacing the file would cut its other hard links (bug B7).
		return t.overwrite(rel, data)
	}
	return atomicfile.WriteIn(t.root.FS(), rel, data, atomicfile.Options{Mode: mode, Umask: !existed, Prefix: ".grok-upload-"})
}

// Download streams the file in chunks. The size is fixed when the download
// starts, so a growing file is cut and a shrinking one ends early.
func (t *Transfer) Download(ctx context.Context, requestID, path string, sink frames.Sink) error {
	f, err := t.root.OpenRegular(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	remaining := st.Size()
	for seq := uint32(0); ; seq++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A fresh buffer per chunk: the sink may hold on to the frame.
		b := make([]byte, min(Chunk, remaining))
		n, err := io.ReadFull(f, b)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return err
		}
		remaining -= int64(n)
		last := remaining == 0 || err != nil
		if err := sink(frames.File(requestID, b[:n], seq, last)); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

// mode returns the permissions of an existing target, with its setuid,
// setgid and sticky bits (bug B7), and its number of hard links; or the
// default for a new file, which also replaces a dangling symlink.
func (t *Transfer) mode(rel string) (mode os.FileMode, links uint64, existed bool, err error) {
	st, err := t.root.FS().Lstat(rel)
	if errors.Is(err, os.ErrNotExist) || err == nil && st.Mode()&os.ModeSymlink != 0 {
		return 0o644, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	if !st.Mode().IsRegular() {
		return 0, 0, false, errors.New("target is not a regular file")
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		links = sys.Nlink
	}
	return st.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky), links, true, nil
}

// overwrite writes data into the existing file rel in place, keeping its
// inode, permissions and hard links; readers may see partial data.
func (t *Transfer) overwrite(rel string, data []byte) error {
	f, err := t.root.FS().OpenFile(rel, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return err
}
