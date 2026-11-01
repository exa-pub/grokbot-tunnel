package transfer_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/transfer"
)

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	st, err := os.Stat(path)
	require.NoError(t, err)
	return st.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}

// New files get 0666 and new directories 0777, less the umask; an existing
// file keeps its mode, even 0000 (F8).
func TestUploadModes(t *testing.T) {
	old := syscall.Umask(0o002)
	defer syscall.Umask(old)
	tr, root := setup(t)
	require.NoError(t, tr.Upload("dir/new", []byte("x")))
	assert.Equal(t, os.FileMode(0o664), mode(t, filepath.Join(root.Dir(), "dir", "new")))
	assert.Equal(t, os.FileMode(0o775), mode(t, filepath.Join(root.Dir(), "dir")))
	locked := filepath.Join(root.Dir(), "locked")
	require.NoError(t, os.WriteFile(locked, []byte("old"), 0o600))
	require.NoError(t, os.Chmod(locked, 0))
	require.NoError(t, tr.Upload("locked", []byte("new")))
	assert.Equal(t, os.FileMode(0), mode(t, locked))
}

func hardLinked(t *testing.T, root *pathroot.Root, name string, perm os.FileMode) (string, string) {
	t.Helper()
	path := filepath.Join(root.Dir(), name)
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	other := filepath.Join(root.Dir(), name+"-link")
	require.NoError(t, os.Link(path, other))
	require.NoError(t, os.Chmod(path, perm))
	return path, other
}

// A file with other hard links is written in place: uploads to it run one
// at a time, its setuid bit survives the write, and a file that cannot be
// opened for writing is replaced instead (F11).
func TestUploadHardLinkedFile(t *testing.T) {
	tr, root := setup(t)
	path, other := hardLinked(t, root, "shared", 0o750|os.ModeSetuid)
	payloads := make([][]byte, 8)
	var wg sync.WaitGroup
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte{byte('a' + i)}, 4<<20)
		wg.Go(func() { assert.NoError(t, tr.Upload("shared", payloads[i])) })
	}
	wg.Wait()
	b, err := os.ReadFile(other)
	require.NoError(t, err)
	require.Len(t, b, 4<<20)
	assert.Equal(t, bytes.Repeat(b[:1], len(b)), b, "uploads mixed")
	assert.Equal(t, 0o750|os.ModeSetuid, mode(t, path))

	if os.Geteuid() != 0 {
		readOnly, _ := hardLinked(t, root, "readonly", 0o444)
		require.NoError(t, tr.Upload("readonly", []byte("new")))
		b, err = os.ReadFile(readOnly)
		require.NoError(t, err)
		assert.Equal(t, "new", string(b))
		assert.Equal(t, os.FileMode(0o444), mode(t, readOnly))
	}
}

// Run as root, an upload keeps the owner of the file it replaces (F9).
func TestUploadKeepsOwnerAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	tr, root := setup(t)
	path := filepath.Join(root.Dir(), "user-file")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))
	require.NoError(t, os.Chown(path, 65534, 65534))
	require.NoError(t, tr.Upload("user-file", []byte("new")))
	st, err := os.Stat(path)
	require.NoError(t, err)
	sys := st.Sys().(*syscall.Stat_t) //nolint:forcetypeassert // Linux
	assert.Equal(t, [2]uint32{65534, 65534}, [2]uint32{sys.Uid, sys.Gid})
}

// Failures read as Node's error texts and name the target, never the
// temporary file (F13).
func TestTransferNodeErrors(t *testing.T) {
	tr, root := setup(t)
	abs := func(name string) string { return filepath.Join(root.Dir(), name) }
	require.NoError(t, os.WriteFile(abs("file"), []byte("x"), 0o600))
	require.NoError(t, os.Mkdir(abs("dir"), 0o700))
	require.EqualError(t, tr.Upload("file/sub", []byte("x")), "EEXIST: file already exists, mkdir '"+abs("file")+"'")
	require.EqualError(t, tr.Upload("dir", []byte("x")), "EISDIR: illegal operation on a directory, rename '"+abs("dir")+"'")
	if os.Geteuid() != 0 {
		require.NoError(t, os.Mkdir(abs("ro"), 0o500))
		t.Cleanup(func() { os.Chmod(abs("ro"), 0o700) }) //nolint:errcheck,gosec // best effort for TempDir cleanup
		require.EqualError(t, tr.Upload("ro/new", []byte("x")), "EACCES: permission denied, open '"+abs("ro/new")+"'")
	}
	sink := func(*aiserverv1.GrokBotUserComputerResponseFrame) error { return nil }
	require.EqualError(t, tr.Download(context.Background(), "r", "missing", sink), "ENOENT: no such file or directory, open '"+abs("missing")+"'")
	require.EqualError(t, tr.Download(context.Background(), "r", "dir", sink), "EISDIR: illegal operation on a directory, read")
}

// A file that reports size 0, as procfs files do, is read to its end; like
// the desktop, an empty frame then ends the download (F12).
func TestDownloadUnknownSize(t *testing.T) {
	root, err := pathroot.Open("/proc/self")
	require.NoError(t, err)
	t.Cleanup(func() { root.Close() })
	var got []byte
	var frames []*aiserverv1.GrokBotUserComputerFile
	err = transfer.New(root).Download(context.Background(), "r", "status", func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
		frames = append(frames, f.GetFile())
		got = append(got, f.GetFile().Data...)
		return nil
	})
	require.NoError(t, err)
	assert.Contains(t, string(got), "Name:")
	require.Len(t, frames, 2)
	assert.True(t, frames[1].Last)
	assert.Empty(t, frames[1].Data)
}
