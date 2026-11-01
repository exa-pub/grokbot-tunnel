package transfer_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/transfer"
)

func setup(t *testing.T) (*transfer.Transfer, *pathroot.Root) {
	t.Helper()
	root, err := pathroot.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return transfer.New(root), root
}

// A new file gets 0666 less the umask, like any file the user creates.
func TestUploadNewFileHonorsUmask(t *testing.T) {
	old := syscall.Umask(0o027)
	defer syscall.Umask(old)
	tr, root := setup(t)
	if err := tr.Upload("new", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(filepath.Join(root.Dir(), "new")); err != nil || st.Mode().Perm() != 0o640 {
		t.Fatal(st.Mode(), err)
	}
	if err := tr.Upload("../escape", []byte("x")); err == nil {
		t.Fatal("upload outside root")
	}
}

func TestDownloadExactBoundariesAndMutation(t *testing.T) {
	tr, root := setup(t)
	for _, size := range []int64{0, 1, transfer.Chunk - 1, transfer.Chunk, transfer.Chunk + 1, 2 * transfer.Chunk, pathroot.MaxFile} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			f, err := os.Create(filepath.Join(root.Dir(), "download"))
			if err != nil {
				t.Fatal(err)
			}
			f.Truncate(size)
			f.Close()
			var chunks int
			var total int
			err = tr.Download(context.Background(), "r", "download", func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
				chunks++
				v := f.GetFile()
				total += len(v.Data)
				if v.Seq != uint32(chunks-1) {
					t.Fatal(v)
				}
				if v.Last && total != int(size) {
					t.Fatal("bad last")
				}
				if len(v.Data) == 0 && size > 0 {
					t.Fatal("extra EOF")
				}
				return nil
			})
			if err != nil || total != int(size) || chunks != max(1, int((size+transfer.Chunk-1)/transfer.Chunk)) {
				t.Fatal(chunks, total, err)
			}
		})
	}
	os.WriteFile(filepath.Join(root.Dir(), "growing"), bytes.Repeat([]byte("x"), transfer.Chunk), 0600)
	err := tr.Download(context.Background(), "r", "growing", func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
		out, _ := os.OpenFile(filepath.Join(root.Dir(), "growing"), os.O_APPEND|os.O_WRONLY, 0600)
		out.WriteString("new")
		out.Close()
		if !f.GetFile().Last {
			t.Fatal("download exceeded initial snapshot")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUploadAtomicModeAndInsideSymlink(t *testing.T) {
	tr, root := setup(t)
	target := filepath.Join(root.Dir(), "target")
	os.WriteFile(target, []byte("old"), 0600)
	os.Chmod(target, 0640)
	if err := os.Symlink("target", filepath.Join(root.Dir(), "link")); err != nil {
		t.Fatal(err)
	}
	if err := tr.Upload("link", []byte("new")); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(target)
	st, _ := os.Stat(target)
	if string(b) != "new" || st.Mode().Perm() != 0640 {
		t.Fatal(string(b), st.Mode())
	}
	if st, _ := os.Lstat(filepath.Join(root.Dir(), "link")); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	if err := tr.Upload("nested/new/file", []byte("created")); err != nil {
		t.Fatal(err)
	}
}

// Invariant 25, Go+: the desktop replaces a dangling symlink with a regular
// file and does not refuse one that points outside the root (rnd-v2,
// SUMMARY row 10). Pinned until aligned.
// D9: like the desktop, an upload writes through a symlink to an existing
// file, but replaces a dangling symlink, wherever it points, with a regular
// file.
func TestUploadThroughDanglingSymlink(t *testing.T) {
	tr, root := setup(t)
	if err := os.Mkdir(filepath.Join(root.Dir(), "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root.Dir(), "sub", "existing"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub/existing", filepath.Join(root.Dir(), "live")); err != nil {
		t.Fatal(err)
	}
	if err := tr.Upload("live", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root.Dir(), "sub", "existing")); err != nil || string(b) != "new" {
		t.Fatal(string(b), err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	for _, target := range []string{"sub/missing", outside} {
		link := filepath.Join(root.Dir(), "dangling")
		_ = os.Remove(link)
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if err := tr.Upload("dangling", []byte("x")); err != nil {
			t.Fatal(target, err)
		}
		if st, err := os.Lstat(link); err != nil || !st.Mode().IsRegular() {
			t.Fatal(target, "dangling symlink not replaced", err)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("file written outside root")
	}
	if _, err := os.Stat(filepath.Join(root.Dir(), "sub", "missing")); err == nil {
		t.Fatal("dangling symlink followed")
	}
}

// Upload keeps the setuid, setgid and sticky bits of the file it replaces,
// and writes a file with other hard links in place, keeping them (bug B7).
func TestUploadKeepsSpecialBitsAndHardLinks(t *testing.T) {
	tr, root := setup(t)
	target := filepath.Join(root.Dir(), "tool")
	if err := os.WriteFile(target, []byte("old"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0o700|os.ModeSetuid|os.ModeSetgid|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	if err := tr.Upload("tool", []byte("new")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(target)
	if err != nil || st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != os.ModeSetuid|os.ModeSetgid|os.ModeSticky || st.Mode().Perm() != 0o700 {
		t.Fatal(st.Mode(), err)
	}
	original := filepath.Join(root.Dir(), "linked")
	if err := os.WriteFile(original, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other-link")
	if err := os.Link(original, other); err != nil {
		t.Skip("no hard links across these directories:", err)
	}
	if err := tr.Upload("linked", []byte("new content")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(other); err != nil || string(b) != "new content" {
		t.Fatal("hard link cut", string(b), err)
	}
	if st, err := os.Stat(original); err != nil || st.Mode().Perm() != 0o640 {
		t.Fatal(st.Mode(), err)
	}
}
