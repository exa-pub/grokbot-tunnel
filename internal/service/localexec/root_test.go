package localexec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

func uploadFrame(path string, data []byte) *aiserverv1.GrokBotUserComputerRequestFrame {
	return &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "u", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Upload{
		Upload: &aiserverv1.GrokBotUserComputerUpload{Path: path, Data: data},
	}}
}

func downloadFrame(path string) *aiserverv1.GrokBotUserComputerRequestFrame {
	return &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "d", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Download{
		Download: &aiserverv1.GrokBotUserComputerDownload{Path: path},
	}}
}

// download returns the data of a download of path, or its file error.
func download(t *testing.T, e *Executor, path string) (data, fileError string) {
	t.Helper()
	for _, f := range collect(t, e, downloadFrame(path)) {
		if f.GetFileError() != nil {
			return "", f.GetFileError().GetError()
		}
		data += string(f.GetFile().GetData())
	}
	return data, ""
}

// newAt opens an executor rooted at root, with shells isolated from the
// user's profile.
func newAt(t *testing.T, root string) *Executor {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ZDOTDIR", t.TempDir())
	e, err := New(root, gate(entity.ModeAlways))
	require.NoError(t, err)
	t.Cleanup(func() { e.Close() })
	return e
}

// readContent reads path and returns its content, or the reason it has
// none: the read result or the throw.
func readContent(t *testing.T, e *Executor, path string) string {
	t.Helper()
	frames := pinnedExec(t, e, `{"readArgs":{"path":"`+path+`"}}`)
	require.NotEmpty(t, frames)
	if c := frames[0].client; c != nil {
		if s := c.GetReadResult().GetSuccess(); s != nil {
			return s.GetContent()
		}
		return c.GetReadResult().String()
	}
	return "throw: " + frames[0].control.GetThrow().GetError()
}

// L3-1: like the desktop's n9, a symlink inside the root whose target is
// inside it is followed by read, ls, download and upload, also when it is
// absolute (~/.vimrc -> $HOME/dotfiles/vimrc, the usual dotfiles link) or
// leaves the root on the way (rel -> ../real/...), which os.Root does not
// follow itself. Results name the path as requested. A symlink out of the
// root is still refused.
func TestSymlinksInsideRoot(t *testing.T) {
	for _, viaLink := range []bool{false, true} {
		t.Run(map[bool]string{false: "plain", true: "symlinkroot"}[viaLink], func(t *testing.T) {
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
			require.NoError(t, os.Symlink("../real/dotfiles", filepath.Join(dir, "reldir")))
			require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(dir, "escape")))
			e := newAt(t, root)

			for _, p := range []string{".vimrc", "dots/vimrc", "rel", "reldir/vimrc", root + "/.vimrc", "dotfiles/vimrc"} {
				frames := pinnedExec(t, e, `{"readArgs":{"path":"`+p+`"}}`)
				require.NotEmpty(t, frames)
				success := frames[0].client.GetReadResult().GetSuccess()
				require.NotNil(t, success, "%s: %+v", p, frames[0])
				assert.Equal(t, "set nu", success.GetContent(), p)
				assert.Equal(t, filepath.Join(root, strings.TrimPrefix(p, root+"/")), success.GetPath(), p)
			}
			frames := pinnedExec(t, e, `{"readArgs":{"path":"dots/missing"}}`)
			require.NotEmpty(t, frames)
			assert.Equal(t, filepath.Join(root, "dots", "missing"), frames[0].client.GetReadResult().GetFileNotFound().GetPath())

			for _, p := range []string{"dots", "reldir"} {
				frames = pinnedExec(t, e, `{"lsArgs":{"path":"`+p+`"}}`)
				require.NotEmpty(t, frames)
				tree := frames[0].client.GetLsResult().GetSuccess().GetDirectoryTreeRoot()
				require.NotNil(t, tree, "%s: %+v", p, frames[0])
				assert.Equal(t, filepath.Join(root, p), tree.GetAbsPath())
				require.Len(t, tree.GetChildrenFiles(), 1, p)
				assert.Equal(t, "vimrc", tree.GetChildrenFiles()[0].GetName(), p)
			}

			for _, p := range []string{".vimrc", "rel", "dots/vimrc"} {
				data, fileError := download(t, e, p)
				assert.Empty(t, fileError, p)
				assert.Equal(t, "set nu", data, p)
			}

			for p, target := range map[string]string{
				"dots/new":          "dotfiles/new",
				"reldir/sub/new":    "dotfiles/sub/new",
				".vimrc":            "dotfiles/vimrc",
				root + "/dots/new2": "dotfiles/new2",
			} {
				frames := pinnedFrames(context.Background(), t, e, uploadFrame(p, []byte(p)))
				require.Len(t, frames, 1)
				require.Nil(t, frames[0].file, p)
				b, err := os.ReadFile(filepath.Join(dir, target))
				require.NoError(t, err, p)
				assert.Equal(t, p, string(b), p)
			}
			fi, err := os.Lstat(filepath.Join(dir, ".vimrc"))
			require.NoError(t, err)
			assert.NotZero(t, fi.Mode()&os.ModeSymlink, "upload kept the symlink")

			const symlink = "Path resolves through a symlink to outside the allowed local-exec root and was refused: "
			text, _ := requireThrow(t, pinnedExec(t, e, `{"readArgs":{"path":"escape/x"}}`))
			assert.Equal(t, symlink+"escape/x", text)
			text, _ = requireThrow(t, pinnedExec(t, e, `{"lsArgs":{"path":"escape"}}`))
			assert.Equal(t, symlink+"escape", text)
			_, fileError := download(t, e, "escape/x")
			assert.Equal(t, symlink+"escape/x", fileError)
			frames = pinnedFrames(context.Background(), t, e, uploadFrame("escape/x", []byte("x")))
			require.Len(t, frames, 1)
			assert.Equal(t, symlink+"escape/x", frames[0].file.GetError())
		})
	}
}

// L3-2: like the desktop, whose root is a path it takes the realpath of on
// every call (n9), the file tools follow a symlink root that is retargeted
// (/srv/app/current -> releases/vN), as the shell does.
func TestRootRetargeted(t *testing.T) {
	base := t.TempDir()
	for _, v := range []string{"v1", "v2"} {
		require.NoError(t, os.MkdirAll(filepath.Join(base, "releases", v), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(base, "releases", v, "VERSION"), []byte(v), 0o600))
	}
	current := filepath.Join(base, "current")
	require.NoError(t, os.Symlink("releases/v1", current))
	e := newAt(t, current)
	assert.Equal(t, "v1", readContent(t, e, "VERSION"))
	assert.Equal(t, "v1", runShell(t, e, "cat VERSION").stdout)

	tmp := filepath.Join(base, "current.tmp")
	require.NoError(t, os.Symlink("releases/v2", tmp))
	require.NoError(t, os.Rename(tmp, current))
	assert.Equal(t, "v2", readContent(t, e, "VERSION"))
	assert.Equal(t, "v2", readContent(t, e, current+"/VERSION"))
	assert.Equal(t, "v2", runShell(t, e, "cat VERSION").stdout)
	frames := pinnedExec(t, e, `{"lsArgs":{"path":"."}}`)
	require.NotEmpty(t, frames)
	assert.Equal(t, current, frames[0].client.GetLsResult().GetSuccess().GetDirectoryTreeRoot().GetAbsPath())
	data, fileError := download(t, e, "VERSION")
	assert.Empty(t, fileError)
	assert.Equal(t, "v2", data)
	frames = pinnedFrames(context.Background(), t, e, uploadFrame("NEW", []byte("x")))
	require.Len(t, frames, 1)
	require.Nil(t, frames[0].file)
	assert.FileExists(t, filepath.Join(base, "releases", "v2", "NEW"))
}

// L3-2: a root removed and created again (rm -rf proj && git clone) is
// opened again, instead of reading the removed directory. While it is
// missing, the file tools fail as the desktop's do on a path under a root
// whose realpath fails (n9): read finds no file, ls no directory, download
// fails to open it, and upload creates the root, like the desktop's
// recursive mkdir.
func TestRootRecreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "proj")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("old"), 0o600))
	e := newAt(t, dir)
	assert.Equal(t, "old", readContent(t, e, "f"))

	require.NoError(t, os.RemoveAll(dir))
	frames := pinnedExec(t, e, `{"readArgs":{"path":"f"}}`)
	require.NotEmpty(t, frames)
	assert.Equal(t, filepath.Join(dir, "f"), frames[0].client.GetReadResult().GetFileNotFound().GetPath())
	frames = pinnedExec(t, e, `{"lsArgs":{"path":"sub"}}`)
	require.NotEmpty(t, frames)
	assert.Equal(t, "Path does not exist: "+filepath.Join(dir, "sub"), frames[0].client.GetLsResult().GetError().GetError())
	_, fileError := download(t, e, "f")
	assert.Equal(t, "ENOENT: no such file or directory, open '"+filepath.Join(dir, "f")+"'", fileError)
	text, _ := requireThrow(t, pinnedExec(t, e, `{"readArgs":{"path":"../x"}}`))
	assert.Equal(t, "Path is outside the allowed local-exec root and was refused: ../x", text)

	frames = pinnedFrames(context.Background(), t, e, uploadFrame("d/up", []byte("up")))
	require.Len(t, frames, 1)
	require.Nil(t, frames[0].file)
	b, err := os.ReadFile(filepath.Join(dir, "d", "up"))
	require.NoError(t, err)
	assert.Equal(t, "up", string(b))

	require.NoError(t, os.RemoveAll(dir))
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f"), []byte("new"), 0o600))
	assert.Equal(t, "new", readContent(t, e, "f"))
	assert.Equal(t, "new", runShell(t, e, "cat f").stdout)
}

// L3-2: reads in parallel while the root is retargeted each read one
// release or the other.
func TestRootRetargetedConcurrently(t *testing.T) {
	base := t.TempDir()
	for _, v := range []string{"v1", "v2"} {
		require.NoError(t, os.MkdirAll(filepath.Join(base, "releases", v), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(base, "releases", v, "VERSION"), []byte(v), 0o600))
	}
	current := filepath.Join(base, "current")
	require.NoError(t, os.Symlink("releases/v1", current))
	e := newAt(t, current)

	stop := make(chan struct{})
	retargeted := make(chan error, 1)
	go func() {
		tmp := filepath.Join(base, "current.tmp")
		for i := 0; ; i++ {
			select {
			case <-stop:
				retargeted <- nil
				return
			default:
			}
			err := os.Symlink([]string{"releases/v2", "releases/v1"}[i%2], tmp)
			if err == nil {
				err = os.Rename(tmp, current)
			}
			if err != nil {
				retargeted <- err
				return
			}
		}
	}()
	var mu sync.Mutex
	got := map[string]int{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 50 {
				var content string
				for _, f := range collect(t, e, execFrame(`{"readArgs":{"path":"VERSION"}}`)) {
					if c := f.GetClient(); c != nil {
						content = c.GetMessageJson()
					}
				}
				mu.Lock()
				switch {
				case strings.Contains(content, `"content":"v1"`):
					got["v1"]++
				case strings.Contains(content, `"content":"v2"`):
					got["v2"]++
				default:
					got[content]++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	close(stop)
	require.NoError(t, <-retargeted)
	assert.Equal(t, 200, got["v1"]+got["v2"], got)
}

// N1: read and ls expand "~" and file:// URLs like the desktop (cr), and so
// do upload and download, where the desktop takes "~" as a directory of
// that name in the root (pinned difference). Results name the expanded
// path. A home directory outside a narrower root is refused, where the
// desktop's n9 checks the unexpanded path and reads it anyway.
func TestFileToolsExpandHome(t *testing.T) {
	root := t.TempDir()
	e := newAt(t, root)
	t.Setenv("HOME", root)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "d"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "d", "f.txt"), []byte("home"), 0o600))

	assert.Equal(t, "home", readContent(t, e, "~/d/f.txt"))
	assert.Equal(t, "home", readContent(t, e, "file://"+root+"/d/f.txt"))
	frames := pinnedExec(t, e, `{"readArgs":{"path":"~/d/f.txt"}}`)
	assert.Equal(t, filepath.Join(root, "d", "f.txt"), frames[0].client.GetReadResult().GetSuccess().GetPath())
	frames = pinnedExec(t, e, `{"readArgs":{"path":"~/d/missing.txt"}}`)
	assert.Equal(t, filepath.Join(root, "d", "missing.txt"), frames[0].client.GetReadResult().GetFileNotFound().GetPath())

	frames = pinnedExec(t, e, `{"lsArgs":{"path":"~/d"}}`)
	tree := frames[0].client.GetLsResult().GetSuccess().GetDirectoryTreeRoot()
	assert.Equal(t, filepath.Join(root, "d"), tree.GetAbsPath())
	require.Len(t, tree.GetChildrenFiles(), 1)
	assert.Equal(t, "f.txt", tree.GetChildrenFiles()[0].GetName())

	for _, f := range collect(t, e, uploadFrame("~/up/new.txt", []byte("uploaded"))) {
		require.Nil(t, f.GetFileError(), f.GetFileError().GetError())
	}
	b, err := os.ReadFile(filepath.Join(root, "up", "new.txt"))
	require.NoError(t, err)
	assert.Equal(t, "uploaded", string(b))
	assert.NoDirExists(t, filepath.Join(root, "~"), "not a directory named ~")
	data, fileError := download(t, e, "~/up/new.txt")
	assert.Empty(t, fileError)
	assert.Equal(t, "uploaded", data)

	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600))
	t.Setenv("HOME", outside)
	assert.Equal(t, "throw: Path is outside the allowed local-exec root and was refused: ~/secret", readContent(t, e, "~/secret"))
	_, fileError = download(t, e, "~/secret")
	assert.Contains(t, fileError, "outside the allowed local-exec root")
}
