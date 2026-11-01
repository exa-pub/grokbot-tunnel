package atomicfile_test

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/atomicfile"
)

func TestWriteReplacesWithMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unit")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	require.NoError(t, atomicfile.Write(path, []byte("new"), atomicfile.Options{Mode: 0o755, SyncDir: true}))
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "new", string(b))
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o755), st.Mode().Perm())
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	assert.Len(t, entries, 1, "temporary file left behind")
}

func TestWriteDefaultModeIgnoresUmask(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.json")
	require.NoError(t, atomicfile.Write(path, []byte("{}"), atomicfile.Options{}))
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}

// A failed check leaves the target untouched and removes the temporary file,
// which the check saw complete under the prefix.
func TestWriteCheckFailureKeepsTarget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bin")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o600))
	fail := errors.New("does not run")
	err := atomicfile.Write(path, []byte("new"), atomicfile.Options{
		Prefix: ".bin.update-",
		Check: func(tmp string) error {
			assert.Equal(t, dir, filepath.Dir(tmp))
			assert.Regexp(t, `^\.bin\.update-`, filepath.Base(tmp))
			b, err := os.ReadFile(tmp)
			require.NoError(t, err)
			assert.Equal(t, "new", string(b))
			return fail
		},
	})
	require.ErrorIs(t, err, fail)
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "old", string(b))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestWriteMissingDirectory(t *testing.T) {
	err := atomicfile.Write(filepath.Join(t.TempDir(), "missing", "f"), nil, atomicfile.Options{})
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestWriteInStaysInRoot(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	require.NoError(t, err)
	defer root.Close()
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(dir, "escape")))
	require.NoError(t, atomicfile.WriteIn(root, "f", []byte("in"), atomicfile.Options{Mode: 0o640, SyncDir: true}))
	b, err := os.ReadFile(filepath.Join(dir, "f"))
	require.NoError(t, err)
	assert.Equal(t, "in", string(b))
	st, err := os.Stat(filepath.Join(dir, "f"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), st.Mode().Perm())
	require.Error(t, atomicfile.WriteIn(root, "escape/f", nil, atomicfile.Options{}))
}

func TestWriteUmask(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	path := filepath.Join(t.TempDir(), "f")
	require.NoError(t, atomicfile.Write(path, nil, atomicfile.Options{Mode: 0o644, Umask: true}))
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), st.Mode().Perm())
}

// ExactMode copies a mode of 0 instead of using the default; Owner keeps
// the owner it is given, here the process's own.
func TestWriteExactModeAndOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	owner := &atomicfile.Owner{UID: os.Geteuid(), GID: os.Getegid()}
	require.NoError(t, atomicfile.Write(path, []byte("x"), atomicfile.Options{ExactMode: true, Owner: owner}))
	st, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0), st.Mode().Perm())
	sys, ok := st.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	assert.Equal(t, uint32(owner.UID), sys.Uid) //nolint:gosec // a uid
}
