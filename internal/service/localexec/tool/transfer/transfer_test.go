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

// A new file gets 0644 less the umask, like any file the user creates.
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
func TestUploadThroughDanglingSymlink(t *testing.T) {
	tr, root := setup(t)
	if err := os.Symlink("sub/target", filepath.Join(root.Dir(), "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root.Dir(), "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := tr.Upload("link", []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root.Dir(), "sub", "target")); err != nil || string(b) != "new" {
		t.Fatal(string(b), err)
	}
	if st, _ := os.Lstat(filepath.Join(root.Dir(), "link")); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced")
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Symlink(outside, filepath.Join(root.Dir(), "escape")); err != nil {
		t.Fatal(err)
	}
	if err := tr.Upload("escape", []byte("x")); err == nil {
		t.Fatal("upload escaped root")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Fatal("file written outside root")
	}
}
