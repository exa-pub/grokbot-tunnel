package pathroot_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

// L3-1: like the desktop's n9, a symlink inside the root whose target is
// inside it passes, also absolute (ln -s "$HOME/dotfiles/vimrc" ~/.vimrc)
// or through ".." above the root, which os.Root refuses to follow: Real is
// the path with them resolved. Also with a root given through a symlink.
func TestCheckSymlinksInsideRoot(t *testing.T) {
	for _, viaLink := range []bool{false, true} {
		base := t.TempDir()
		dir := filepath.Join(base, "real")
		require.NoError(t, os.MkdirAll(filepath.Join(dir, "dotfiles"), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "dotfiles", "vimrc"), []byte("set nu"), 0o600))
		root := dir
		if viaLink {
			root = filepath.Join(base, "home")
			require.NoError(t, os.Symlink(dir, root))
		}
		require.NoError(t, os.Symlink(filepath.Join(root, "dotfiles", "vimrc"), filepath.Join(dir, ".vimrc")))
		require.NoError(t, os.Symlink(filepath.Join(root, "dotfiles"), filepath.Join(dir, "dots")))
		require.NoError(t, os.Symlink("../real/dotfiles/vimrc", filepath.Join(dir, "rel")))
		require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(dir, "escape")))
		r, err := pathroot.Open(root)
		require.NoError(t, err)
		defer r.Close()

		for in, want := range map[string]string{
			".vimrc":             "dotfiles/vimrc",
			"dots/vimrc":         "dotfiles/vimrc",
			"rel":                "dotfiles/vimrc",
			"dots":               "dotfiles",
			"dotfiles/vimrc":     "dotfiles/vimrc",
			root + "/dots/new/f": "dotfiles/new/f",
		} {
			p := check(t, r, in)
			rel, err := r.Relative(in)
			require.NoError(t, err)
			assert.Equal(t, rel, p.Rel, in)
			assert.Equal(t, want, p.Real, in)
			_, err = p.FS().Lstat(p.Real)
			if want == "dotfiles/new/f" {
				require.ErrorIs(t, err, os.ErrNotExist, in)
			} else {
				require.NoError(t, err, in)
			}
		}
		f, err := openRegular(r, ".vimrc")
		require.NoError(t, err)
		f.Close()
		_, err = r.Check("escape/x")
		require.EqualError(t, err, "Path resolves through a symlink to outside the allowed local-exec root and was refused: escape/x")
	}
}

// release creates releases/<v>/VERSION below base.
func release(t *testing.T, base, v string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Join(base, "releases", v), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(base, "releases", v, "VERSION"), []byte(v), 0o600))
}

// retarget points the symlink link at target atomically, as ln -sfn and
// deploy tools do.
func retarget(link, target string) error {
	tmp := link + ".tmp"
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

// readOnce reads the file path below r.
func readOnce(r *pathroot.Root, path string) (string, error) {
	p, err := r.Check(path)
	if err != nil {
		return "", err
	}
	defer p.Close()
	f, err := p.OpenRegular()
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	return string(b), err
}

func readPath(t *testing.T, r *pathroot.Root, path string) string {
	t.Helper()
	got, err := readOnce(r, path)
	require.NoError(t, err, path)
	return got
}

// L3-2: like the desktop, whose root is a path it takes the realpath of on
// every call (n9), the root follows its symlink when it is retargeted; a
// path checked before keeps the directory it was checked in until it is
// closed.
func TestRootFollowsRetarget(t *testing.T) {
	base := t.TempDir()
	release(t, base, "v1")
	release(t, base, "v2")
	current := filepath.Join(base, "current")
	require.NoError(t, os.Symlink("releases/v1", current))
	r, err := pathroot.Open(current)
	require.NoError(t, err)
	defer r.Close()
	assert.Equal(t, "v1", readPath(t, r, "VERSION"))
	held, err := r.Check("VERSION")
	require.NoError(t, err)

	require.NoError(t, retarget(current, "releases/v2"))
	assert.Equal(t, "v2", readPath(t, r, "VERSION"))
	assert.Equal(t, "v2", readPath(t, r, current+"/VERSION"))
	_, err = r.Check(filepath.Join(base, "releases", "v2", "VERSION"))
	require.ErrorIs(t, err, pathroot.ErrOutside)
	require.NoError(t, r.MkdirAll("terminals", 0o700))
	assert.DirExists(t, filepath.Join(base, "releases", "v2", "terminals"))

	// The old snapshot stays open for the path checked in it, and is
	// closed with it.
	f, err := held.OpenRegular()
	require.NoError(t, err)
	b, err := io.ReadAll(f)
	f.Close()
	require.NoError(t, err)
	assert.Equal(t, "v1", string(b))
	fs := held.FS()
	held.Close()
	_, err = fs.Stat(".")
	require.ErrorIs(t, err, os.ErrClosed)

	require.NoError(t, retarget(current, "releases/v1"))
	assert.Equal(t, "v1", readPath(t, r, "VERSION"))
}

// L3-2: a root removed and created again (rm -rf proj && git clone) is
// opened again. While it is missing, a path below it fails to open, as the
// desktop's file tools fail on it once its realpath check is skipped.
func TestRootRecreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("old"), 0o600))
	r, err := pathroot.Open(dir)
	require.NoError(t, err)
	defer r.Close()
	assert.Equal(t, "old", readPath(t, r, "f"))

	require.NoError(t, os.RemoveAll(dir))
	_, err = r.Check("f")
	var unavailable *pathroot.RootError
	require.ErrorAs(t, err, &unavailable)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, filepath.Join(dir, "f"), unavailable.Path)
	require.EqualError(t, err, "open "+filepath.Join(dir, "f")+": no such file or directory")
	_, err = r.Check("../x")
	require.ErrorIs(t, err, pathroot.ErrOutside)
	require.ErrorIs(t, r.MkdirAll("terminals", 0o700), os.ErrNotExist)

	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("new"), 0o600))
	assert.Equal(t, "new", readPath(t, r, "f"))

	// A file in its place is not a directory.
	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.WriteFile(dir, nil, 0o600))
	_, err = r.Check("f")
	require.ErrorAs(t, err, &unavailable)
	require.ErrorIs(t, err, syscall.ENOTDIR)

	require.NoError(t, r.Close())
	_, err = r.Check("f")
	require.ErrorIs(t, err, os.ErrClosed)
}

// L3-2: requests running in parallel while the root is retargeted each see
// one release or the other, never a refusal or a closed root.
func TestRootRetargetConcurrent(t *testing.T) {
	base := t.TempDir()
	release(t, base, "v1")
	release(t, base, "v2")
	current := filepath.Join(base, "current")
	require.NoError(t, os.Symlink("releases/v1", current))
	r, err := pathroot.Open(current)
	require.NoError(t, err)
	defer r.Close()

	stop := make(chan struct{})
	retargeted := make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				retargeted <- nil
				return
			default:
			}
			if err := retarget(current, "releases/v"+strconv.Itoa(2-i%2)); err != nil {
				retargeted <- err
				return
			}
		}
	}()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			for range 300 {
				got, err := readOnce(r, "VERSION")
				if err == nil && got != "v1" && got != "v2" {
					err = fmt.Errorf("read %q", got)
				}
				if err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	require.NoError(t, <-retargeted)
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
