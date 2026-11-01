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
	"sync"
	"syscall"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/atomicfile"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/nodeerr"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

// Chunk is the size of a download chunk.
const Chunk = 1 << 20

// Transfer uploads and downloads files below a root.
type Transfer struct {
	root  *pathroot.Root
	locks pathLocks
}

// New returns a Transfer for files below root.
func New(root *pathroot.Root) *Transfer { return &Transfer{root: root} }

// Upload writes data to path, following symlinks inside the root, as the
// desktop's atomic write (f0) does: uploads to one file run one at a time;
// an existing file keeps its mode, a new one gets 0666 and new directories
// 0777, less the umask. Readers never see partial data, except in a file
// with other hard links, which is written in place to keep them (bug B7).
// Errors are worded as Node's and name the target, not the temporary file.
func (t *Transfer) Upload(path string, data []byte) error {
	if len(data) > pathroot.MaxFile {
		return pathroot.FileTooLarge(int64(len(data)))
	}
	p, err := t.root.Check(path)
	var unavailable *pathroot.RootError
	if errors.As(err, &unavailable) && errors.Is(err, os.ErrNotExist) {
		// Like the desktop's recursive mkdir, the upload creates a root
		// directory that was removed.
		if err = os.MkdirAll(t.root.Dir(), 0o777); err != nil { //nolint:gosec // like the desktop, 0777 less the umask
			return nodeerr.Wrap(err, t.root.Dir())
		}
		p, err = t.root.Check(path)
	}
	if errors.As(err, &unavailable) {
		return nodeerr.Wrap(unavailable.Err, unavailable.Path)
	}
	if err != nil {
		return err
	}
	defer p.Close()
	fs := p.FS()
	// Like the desktop, the directory is created before the file's own
	// symlink resolves.
	if err = fs.MkdirAll(filepath.Dir(p.Real), 0o777); err != nil {
		return t.wrap(err, filepath.Dir(p.Rel))
	}
	rel, err := p.UploadTarget()
	if errors.Is(err, pathroot.ErrOutside) {
		return &pathroot.OutsideError{Path: path, Symlink: true}
	} else if err != nil {
		return t.wrap(err, p.Rel)
	}
	defer t.locks.lock(rel)()
	target, err := t.target(fs, rel)
	if err != nil {
		return t.wrap(err, rel)
	}
	if target.links > 1 {
		opened, err := t.overwrite(fs, rel, data, target.mode)
		if opened || !errors.Is(err, os.ErrPermission) {
			return t.wrap(err, rel)
		}
		// A file we may not write into can still be replaced, as on the
		// desktop, at the cost of its other links.
	}
	return t.wrap(atomicfile.WriteIn(fs, rel, data, atomicfile.Options{
		Mode: target.mode, ExactMode: target.existed, Umask: !target.existed, Owner: target.owner, Prefix: ".grok-upload-",
	}), rel)
}

// Download streams the file in chunks. Like the desktop, a file whose size
// is known is read up to that size; a file that reports size 0, such as one
// in procfs, is read to its end, up to the transfer limit.
func (t *Transfer) Download(ctx context.Context, requestID, path string, sink frames.Sink) error {
	p, err := t.root.Check(path)
	var unavailable *pathroot.RootError
	if errors.As(err, &unavailable) {
		return nodeerr.Wrap(unavailable.Err, unavailable.Path)
	}
	if err != nil {
		return err
	}
	defer p.Close()
	rel := p.Rel
	f, size, err := t.openDownload(p)
	if err != nil {
		return err
	}
	defer f.Close()
	sent := int64(0)
	for seq := uint32(0); ; seq++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := int64(Chunk)
		if size > 0 {
			n = min(n, size-sent)
		}
		// A fresh buffer per chunk: the sink may hold on to the frame.
		b := make([]byte, n)
		m, err := io.ReadFull(f, b)
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return t.wrap(err, rel)
		}
		if sent += int64(m); sent > pathroot.MaxFile {
			return pathroot.FileTooLarge(sent)
		}
		last := m == 0 || sent == size
		if err := sink(frames.File(requestID, b[:m], seq, last)); err != nil {
			return err
		}
		if last {
			return nil
		}
	}
}

// wrap words err as Node would for the file rel, relative to the root's
// path or to its realpath, which name the same directory.
func (t *Transfer) wrap(err error, rel string) error {
	if pathroot.Escaped(err) {
		return err
	}
	return nodeerr.Wrap(err, t.root.Abs(rel))
}

// uploadTarget is what an upload keeps of the file it replaces.
type uploadTarget struct {
	mode    os.FileMode
	links   uint64
	existed bool
	owner   *atomicfile.Owner
}

// target returns the mode of an existing target, with its setuid, setgid
// and sticky bits (bug B7), its number of hard links and, for a root
// process, its owner; or 0666 for a new file, which also replaces a dangling
// symlink. Like the desktop, an upload replaces a FIFO or socket but not a
// directory.
func (t *Transfer) target(fs *os.Root, rel string) (uploadTarget, error) {
	st, err := fs.Lstat(rel)
	switch {
	case errors.Is(err, os.ErrNotExist), err == nil && st.Mode()&os.ModeSymlink != 0:
		return uploadTarget{mode: 0o666}, nil
	case err != nil:
		return uploadTarget{}, err
	case st.IsDir():
		return uploadTarget{}, &os.LinkError{Op: "rename", Old: rel, New: rel, Err: syscall.EISDIR}
	}
	target := uploadTarget{mode: st.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky), existed: true}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		if st.Mode().IsRegular() {
			target.links = uint64(sys.Nlink) //nolint:unconvert // uint32 on arm64, uint64 on amd64
		}
		if os.Geteuid() == 0 {
			target.owner = &atomicfile.Owner{UID: int(sys.Uid), GID: int(sys.Gid)}
		}
	}
	return target, nil
}

// overwrite writes data into the existing file rel in place, keeping its
// inode, owner and hard links, and restores its mode, which the write may
// have cleared setuid and setgid of; readers may see partial data. It
// reports whether the file could be opened.
func (t *Transfer) overwrite(fs *os.Root, rel string, data []byte, mode os.FileMode) (bool, error) {
	f, err := fs.OpenFile(rel, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return false, err
	}
	if _, err = f.Write(data); err == nil {
		err = keepChmodRefusal(f.Chmod(mode))
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	return true, err
}

// keepChmodRefusal ignores a chmod that the file system or, for another
// user's file, the kernel refuses, as the desktop does (iMe).
func keepChmodRefusal(err error) error {
	switch {
	case errors.Is(err, syscall.ENOSYS):
		return nil
	case os.Geteuid() != 0 && (errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL)):
		return nil
	}
	return err
}

// openDownload opens a regular file for a download and returns its size. A
// directory fails as Node's read of it does; a FIFO or device is refused
// without blocking.
func (t *Transfer) openDownload(p *pathroot.Path) (*os.File, int64, error) {
	rel := p.Rel
	f, err := p.FS().OpenFile(p.Real, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, 0, t.wrap(err, rel)
	}
	st, err := f.Stat()
	switch {
	case err != nil:
		err = t.wrap(err, rel)
	case st.IsDir():
		err = t.wrap(&os.PathError{Op: "read", Path: rel, Err: syscall.EISDIR}, rel)
	case !st.Mode().IsRegular():
		err = errors.New("expected regular file")
	case st.Size() > pathroot.MaxFile:
		err = pathroot.FileTooLarge(st.Size())
	}
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, st.Size(), nil
}

// pathLocks serializes uploads to one file, like the desktop's per-path
// queue (kBt), so that two uploads cannot mix their data.
type pathLocks struct {
	mu    sync.Mutex
	paths map[string]*pathLock
}

type pathLock struct {
	sync.Mutex
	users int
}

// lock waits for the lock of path and returns its release.
func (p *pathLocks) lock(path string) func() {
	p.mu.Lock()
	if p.paths == nil {
		p.paths = map[string]*pathLock{}
	}
	l := p.paths[path]
	if l == nil {
		l = &pathLock{}
		p.paths[path] = l
	}
	l.users++
	p.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		p.mu.Lock()
		if l.users--; l.users == 0 {
			delete(p.paths, path)
		}
		p.mu.Unlock()
	}
}
