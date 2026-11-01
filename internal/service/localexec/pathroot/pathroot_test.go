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

// D4: like the desktop, any existing directory is a working directory, also
// outside the root; "~", file:// URLs and paths relative to the root are
// expanded. A missing directory or a file runs in the root with a warning.
func TestCwd(t *testing.T) {
	r := open(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	outside := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(r.Dir(), "d"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(r.Dir(), "f"), nil, 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(r.Dir(), "escape")))
	for in, want := range map[string]string{
		"":                  r.Dir(),
		"d":                 filepath.Join(r.Dir(), "d"),
		" d ":               filepath.Join(r.Dir(), "d"),
		"/":                 "/",
		outside:             outside,
		"escape":            filepath.Join(r.Dir(), "escape"),
		"..":                filepath.Dir(r.Dir()),
		"~":                 home,
		"file://" + outside: outside,
	} {
		dir, warning := r.Cwd(in)
		assert.Equal(t, want, dir, in)
		assert.Empty(t, warning, in)
	}
	for _, in := range []string{"missing", "f", "/nonexistent"} {
		dir, warning := r.Cwd(in)
		assert.Equal(t, r.Dir(), dir, in)
		assert.Equal(t, "working directory "+in+" does not exist on this machine; running in "+r.Dir()+" instead\n", warning)
	}
	assert.True(t, r.IsDir("d"))
	assert.True(t, r.IsDir("/"))
	assert.False(t, r.IsDir("f"))
	assert.False(t, r.IsDir(""))
}

func TestCwdState(t *testing.T) {
	r := open(t)
	require.NoError(t, os.WriteFile(filepath.Join(r.Dir(), "f"), nil, 0o600))
	for in, want := range map[string]string{"": "exists", "/": "exists", "nope": "missing", "f": "missing"} {
		got := r.CwdState(in)
		require.NotNil(t, got, in)
		assert.Equal(t, want, *got, in)
	}
}

// D5: paths outside the root are refused in the desktop's words, lexically
// and through a symlink, with the first line of its stack trace.
func TestCheck(t *testing.T) {
	r := open(t)
	outside := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(r.Dir(), "d"), 0o700))
	require.NoError(t, os.Symlink(outside, filepath.Join(r.Dir(), "escape")))
	require.NoError(t, os.Symlink("d", filepath.Join(r.Dir(), "inside")))
	for in, want := range map[string]string{"inside/new/file": "inside/new/file", "d/missing": "d/missing", "..foo": "..foo"} {
		rel, err := r.Check(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, rel, in)
	}
	_, err := r.Check("../x")
	require.ErrorIs(t, err, pathroot.ErrOutside)
	require.EqualError(t, err, "Path is outside the allowed local-exec root and was refused: ../x")
	for _, in := range []string{"escape", "escape/missing/deeper"} {
		_, err = r.Check(in)
		require.ErrorIs(t, err, pathroot.ErrOutside, in)
		require.EqualError(t, err, "Path resolves through a symlink to outside the allowed local-exec root and was refused: "+in)
		var traced interface{ StackTrace() string }
		require.ErrorAs(t, err, &traced)
		assert.Equal(t, "SandLocalExecPathError: "+err.Error(), traced.StackTrace())
	}
}
