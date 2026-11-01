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

// check checks path, which must pass, and closes it with the test.
func check(t *testing.T, r *pathroot.Root, path string) *pathroot.Path {
	t.Helper()
	p, err := r.Check(path)
	require.NoError(t, err, path)
	t.Cleanup(p.Close)
	return p
}

// openRegular opens the regular file path below r.
func openRegular(r *pathroot.Root, path string) (*os.File, error) {
	p, err := r.Check(path)
	if err != nil {
		return nil, err
	}
	defer p.Close()
	return p.OpenRegular()
}

// uploadTarget returns the upload target of rel below r.
func uploadTarget(t *testing.T, r *pathroot.Root, rel string) (string, error) {
	t.Helper()
	return check(t, r, rel).UploadTarget()
}

// L2-1: like the desktop (ult, q9), a root given through a symlink is kept
// as it is: paths are checked lexically against it, and only their realpath
// against its realpath. Its target is outside the root.
func TestOpenKeepsSymlinks(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "001")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(base, "002"), 0o700))
	link := filepath.Join(base, "002", "home")
	require.NoError(t, os.Symlink(dir, link))
	outside := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "d"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "d", "f"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink("d/f", filepath.Join(dir, "live")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "escape")))

	r, err := pathroot.Open(link + "/")
	require.NoError(t, err)
	defer r.Close()
	assert.Equal(t, link, r.Dir())
	assert.Equal(t, filepath.Join(link, "a"), r.Abs("a"))

	for in, want := range map[string]string{"d/f": "d/f", link + "/d/f": "d/f", "d/new": "d/new", link: "."} {
		assert.Equal(t, want, check(t, r, in).Rel, in)
	}
	f, err := openRegular(r, link+"/d/f")
	require.NoError(t, err)
	f.Close()

	for _, in := range []string{dir + "/d/f", dir} {
		_, err = r.Check(in)
		require.EqualError(t, err, "Path is outside the allowed local-exec root and was refused: "+in)
	}
	for _, in := range []string{"escape", link + "/escape/x"} {
		_, err = r.Check(in)
		require.EqualError(t, err, "Path resolves through a symlink to outside the allowed local-exec root and was refused: "+in)
	}

	got, err := uploadTarget(t, r, "live")
	require.NoError(t, err)
	assert.Equal(t, "d/f", got)

	for in, want := range map[string]string{"": link, "d": filepath.Join(link, "d")} {
		cwd, warning := r.Cwd(in)
		assert.Equal(t, want, cwd, in)
		assert.Empty(t, warning, in)
	}
	cwd, warning := r.Cwd("missing")
	assert.Equal(t, link, cwd)
	assert.Equal(t, "working directory missing does not exist on this machine; running in "+link+" instead\n", warning)
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
	fs, err := os.OpenRoot(r.Dir())
	require.NoError(t, err)
	defer fs.Close()
	_, err = fs.Stat("escape/x")
	assert.True(t, pathroot.Escaped(err))
	_, err = fs.Stat("missing")
	assert.False(t, pathroot.Escaped(err))
	assert.False(t, pathroot.Escaped(nil))
}

func TestOpenRegular(t *testing.T) {
	r := open(t)
	require.NoError(t, os.WriteFile(filepath.Join(r.Dir(), "f"), []byte("x"), 0o600))
	f, err := openRegular(r, "f")
	require.NoError(t, err)
	f.Close()

	require.NoError(t, syscall.Mkfifo(filepath.Join(r.Dir(), "fifo"), 0o600))
	_, err = openRegular(r, "fifo")
	require.EqualError(t, err, "expected regular file")

	big, err := os.Create(filepath.Join(r.Dir(), "big"))
	require.NoError(t, err)
	require.NoError(t, big.Truncate(pathroot.MaxFile+1)) // sparse
	big.Close()
	_, err = openRegular(r, "big")
	require.ErrorContains(t, err, "File is 100.0 MiB, which exceeds Grok Bot's 100.0 MiB limit")

	_, err = openRegular(r, "../x")
	require.ErrorIs(t, err, pathroot.ErrOutside)
}

func TestUploadTarget(t *testing.T) {
	r := open(t)
	outside := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(r.Dir(), "d"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(r.Dir(), "d", "file"), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "x"), nil, 0o600))
	require.NoError(t, os.Symlink("d/file", filepath.Join(r.Dir(), "live")))
	require.NoError(t, os.Symlink("d/target", filepath.Join(r.Dir(), "dangling")))
	require.NoError(t, os.Symlink(filepath.Join(outside, "missing"), filepath.Join(r.Dir(), "dangling-out")))
	require.NoError(t, os.Symlink("loop", filepath.Join(r.Dir(), "loop")))

	// A dangling symlink is replaced, as on the desktop.
	for in, want := range map[string]string{"new": "new", "live": "d/file", "dangling": "dangling", "dangling-out": "dangling-out", "d": "d"} {
		got, err := uploadTarget(t, r, in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	// A symlink out of the root swapped in after the check.
	out := check(t, r, "out")
	require.NoError(t, os.Symlink(filepath.Join(outside, "x"), filepath.Join(r.Dir(), "out")))
	_, err := out.UploadTarget()
	require.ErrorIs(t, err, pathroot.ErrOutside)
	_, err = uploadTarget(t, r, "loop")
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

// L8: like the desktop's flt, ENOENT and ENOTDIR (a path through a file)
// are "missing"; other stat failures leave the state out.
func TestCwdState(t *testing.T) {
	r := open(t)
	require.NoError(t, os.WriteFile(filepath.Join(r.Dir(), "f"), nil, 0o600))
	for in, want := range map[string]string{"": "exists", "/": "exists", "nope": "missing", "f": "missing", "f/sub": "missing"} {
		got := r.CwdState(in)
		require.NotNil(t, got, in)
		assert.Equal(t, want, *got, in)
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(r.Dir(), "locked")
		require.NoError(t, os.Mkdir(locked, 0o700))
		require.NoError(t, os.Chmod(locked, 0))
		t.Cleanup(func() { os.Chmod(locked, 0o700) })
		assert.Nil(t, r.CwdState("locked/sub"))
	}
}

// L9: like the desktop's ilt, a requested working directory is resolved
// before the command runs: blank is the session's cwd, a missing one falls
// back to the root with a warning, and other failures are errors in the
// words of Node's fs.
func TestResolveCwd(t *testing.T) {
	r := open(t)
	require.NoError(t, os.Mkdir(filepath.Join(r.Dir(), "d"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(r.Dir(), "f"), nil, 0o600))
	for in, want := range map[string]string{"": "", "  ": "", "d": filepath.Join(r.Dir(), "d"), "/": "/"} {
		dir, warning, err := r.ResolveCwd(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, dir, in)
		assert.Empty(t, warning, in)
	}
	for _, in := range []string{"missing", "f", "f/sub", " missing "} {
		dir, warning, err := r.ResolveCwd(in)
		require.NoError(t, err, in)
		assert.Equal(t, r.Dir(), dir, in)
		assert.Equal(t, "working directory "+in+" does not exist on this machine; running in "+r.Dir()+" instead\n", warning)
	}
	if os.Geteuid() != 0 {
		locked := filepath.Join(r.Dir(), "locked")
		require.NoError(t, os.Mkdir(locked, 0o700))
		require.NoError(t, os.Chmod(locked, 0))
		t.Cleanup(func() { os.Chmod(locked, 0o700) })
		_, _, err := r.ResolveCwd("locked/sub")
		require.ErrorIs(t, err, syscall.EACCES)
		assert.EqualError(t, err, "EACCES: permission denied, stat '"+filepath.Join(locked, "sub")+"'")
	}
}

// L10: an os.Root refusal that Check could not foresee, such as through a
// dangling symlink, becomes the desktop's symlink refusal.
func TestRefuse(t *testing.T) {
	r := open(t)
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(r.Dir(), "dangling")))
	p := check(t, r, "dangling")
	_, err := p.FS().Stat(p.Real)
	require.True(t, pathroot.Escaped(err))
	err = pathroot.Refuse("dangling", err)
	require.ErrorIs(t, err, pathroot.ErrOutside)
	require.EqualError(t, err, "Path resolves through a symlink to outside the allowed local-exec root and was refused: dangling")
	other := os.ErrNotExist
	assert.Same(t, other, pathroot.Refuse("x", other))
	assert.NoError(t, pathroot.Refuse("x", nil))
}

// D5: paths outside the root are refused in the desktop's words, lexically
// and through a symlink, with the first line of its stack trace.
func TestCheck(t *testing.T) {
	r := open(t)
	outside := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(r.Dir(), "d"), 0o700))
	require.NoError(t, os.Symlink(outside, filepath.Join(r.Dir(), "escape")))
	require.NoError(t, os.Symlink("d", filepath.Join(r.Dir(), "inside")))
	for in, want := range map[string][2]string{
		"inside/new/file": {"inside/new/file", "d/new/file"},
		"d/missing":       {"d/missing", "d/missing"},
		"..foo":           {"..foo", "..foo"},
	} {
		p := check(t, r, in)
		assert.Equal(t, want, [2]string{p.Rel, p.Real}, in)
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

// N1: like the desktop's read and ls (cr: kOe, FNt), "~" and file:// URLs are
// expanded; "~user" and a URL with a host stay as they are. The expanded
// path is checked against the root, unlike the desktop's q9, which checks
// <root>/~/secret and so lets read leave a root without the home directory.
func TestRelativeExpandsHome(t *testing.T) {
	r := open(t)
	t.Setenv("HOME", r.Dir())
	for in, want := range map[string]string{
		"~":                            ".",
		"~/":                           ".",
		"~/a/f":                        filepath.Join("a", "f"),
		"file://" + r.Dir() + "/a/f":   filepath.Join("a", "f"),
		"file://" + r.Dir() + "/a%20b": "a b",
		"~user/f":                      filepath.Join("~user", "f"),
		"a/~/f":                        filepath.Join("a", "~", "f"),
		"file://host" + r.Dir() + "/f": filepath.Join("file:", "host"+r.Dir(), "f"),
	} {
		rel, err := r.Relative(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, rel, in)
	}

	t.Setenv("HOME", t.TempDir()) // the home directory is outside the root
	for _, in := range []string{"~/secret", "~", "file://" + os.Getenv("HOME") + "/secret"} {
		_, err := r.Relative(in)
		var outside *pathroot.OutsideError
		require.ErrorAs(t, err, &outside, in)
		assert.Equal(t, in, outside.Path, "the refusal names the path as requested")
	}
}
