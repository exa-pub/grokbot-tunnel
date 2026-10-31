package pathroot_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

func open(t *testing.T) *pathroot.Root {
	t.Helper()
	r, err := pathroot.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { r.Close() })
	return r
}

func TestOpenResolvesSymlinks(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(dir, link))
	r, err := pathroot.Open(link)
	require.NoError(t, err)
	defer r.Close()
	want, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	assert.Equal(t, want, r.Dir())
	assert.Equal(t, filepath.Join(want, "a"), r.Abs("a"))
}

func TestRelative(t *testing.T) {
	r := open(t)
	for in, want := range map[string]string{"": ".", ".": ".", "a/../b": "b", r.Dir() + "/x": "x", "..foo": "..foo"} {
		got, err := r.Relative(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"..", "../x", "/", "a/../../x"} {
		_, err := r.Relative(in)
		require.ErrorIs(t, err, pathroot.ErrOutside, in)
	}
}

func TestEscaped(t *testing.T) {
	r := open(t)
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(r.Dir(), "escape")))
	_, err := r.FS().Stat("escape/x")
	assert.True(t, pathroot.Escaped(err))
	_, err = r.FS().Stat("missing")
	assert.False(t, pathroot.Escaped(err))
	assert.False(t, pathroot.Escaped(nil))
}

func TestCwd(t *testing.T) {
	r := open(t)
	require.NoError(t, os.Mkdir(filepath.Join(r.Dir(), "d"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(r.Dir(), "f"), nil, 0o600))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(r.Dir(), "escape")))

	dir, warning, err := r.Cwd("d")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(r.Dir(), "d"), dir)
	assert.Empty(t, warning)

	dir, warning, err = r.Cwd("missing")
	require.NoError(t, err)
	assert.Equal(t, r.Dir(), dir)
	assert.Equal(t, "working directory missing does not exist on this machine; running in "+r.Dir()+" instead\n", warning)

	_, _, err = r.Cwd("f")
	require.EqualError(t, err, "working directory is not a directory")
	_, _, err = r.Cwd("/")
	require.ErrorIs(t, err, pathroot.ErrOutside)
	_, _, err = r.Cwd("escape")
	require.ErrorIs(t, err, pathroot.ErrOutside)

	assert.True(t, r.IsDir("d"))
	assert.False(t, r.IsDir("f"))
	assert.False(t, r.IsDir("/"))
}

func TestCwdState(t *testing.T) {
	r := open(t)
	require.NoError(t, os.WriteFile(filepath.Join(r.Dir(), "f"), nil, 0o600))
	assert.Equal(t, "exists", *r.CwdState(""))
	assert.Equal(t, "missing", *r.CwdState("nope"))
	assert.Nil(t, r.CwdState("f"))
	assert.Nil(t, r.CwdState("/"))
}

func TestOpenRegular(t *testing.T) {
	r := open(t)
	require.NoError(t, os.WriteFile(filepath.Join(r.Dir(), "f"), []byte("x"), 0o600))
	f, err := r.OpenRegular("f")
	require.NoError(t, err)
	f.Close()

	require.NoError(t, syscall.Mkfifo(filepath.Join(r.Dir(), "fifo"), 0o600))
	_, err = r.OpenRegular("fifo")
	require.EqualError(t, err, "expected regular file")

	big, err := os.Create(filepath.Join(r.Dir(), "big"))
	require.NoError(t, err)
	require.NoError(t, big.Truncate(pathroot.MaxFile+1)) // sparse
	big.Close()
	_, err = r.OpenRegular("big")
	require.ErrorContains(t, err, "File is 100.0 MiB, which exceeds Grok Bot's 100.0 MiB limit")

	_, err = r.OpenRegular("../x")
	require.ErrorIs(t, err, pathroot.ErrOutside)
}

func TestUploadTarget(t *testing.T) {
	r := open(t)
	outside := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(r.Dir(), "d"), 0o700))
	require.NoError(t, os.Symlink("d/target", filepath.Join(r.Dir(), "dangling")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "x"), filepath.Join(r.Dir(), "out")))
	require.NoError(t, os.Symlink("loop", filepath.Join(r.Dir(), "loop")))

	for in, want := range map[string]string{"new": "new", "dangling": "d/target", "d": "d"} {
		got, err := r.UploadTarget(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	_, err := r.UploadTarget("out")
	require.ErrorIs(t, err, pathroot.ErrOutside)
	_, err = r.UploadTarget("loop")
	require.Error(t, err)
}
