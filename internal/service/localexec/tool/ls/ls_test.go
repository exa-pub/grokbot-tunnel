package ls

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

func openRoot(t *testing.T) *pathroot.Root {
	t.Helper()
	root, err := pathroot.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	return root
}

func list(t *testing.T, root *pathroot.Root, path string) *agentv1.LsResult {
	t.Helper()
	var got *agentv1.ExecClientMessage
	err := New(root).Execute(context.Background(), &agentv1.LsArgs{Path: path}, func(m *agentv1.ExecClientMessage) error {
		got = m
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got.GetLsResult()
}

// Invalid UTF-8 in a file name cannot be encoded as JSON; it is replaced.
func TestListInvalidUTF8Name(t *testing.T) {
	root := openRoot(t)
	if err := os.MkdirAll(filepath.Join(root.Dir(), "d\xff"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"caf\xe9.txt", "d\xff/x.\xfe"} {
		if err := os.WriteFile(filepath.Join(root.Dir(), name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tree := list(t, root, ".").GetSuccess().GetDirectoryTreeRoot()
	if tree == nil || len(tree.ChildrenFiles) != 1 || tree.ChildrenFiles[0].Name != "caf�.txt" ||
		len(tree.ChildrenDirs) != 1 || !strings.HasSuffix(tree.ChildrenDirs[0].AbsPath, "d�") {
		t.Fatal(tree)
	}
	tree = &treeNode{FullSubtreeExtensionCounts: map[string]int32{".\xfe": 1, ".\xff": 2}}
	validUTF8(tree)
	if tree.FullSubtreeExtensionCounts[".�"] != 3 {
		t.Fatal(tree)
	}
}

func makeDirs(t *testing.T, root *pathroot.Root, prefix string, n int, file string) {
	t.Helper()
	for i := range n {
		dir := filepath.Join(root.Dir(), fmt.Sprintf("%s%03d", prefix, i))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, file), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func countTree(n *treeNode) (nodes, listed int) {
	nodes, listed = 1, len(n.ChildrenFiles)
	for _, child := range n.ChildrenDirs {
		cn, cl := countTree(child)
		nodes, listed = nodes+cn, listed+cl
	}
	return nodes, listed
}

// Like the desktop, only the root path and file names are charged to the
// budget; directories are free (F2).
func TestListBudgetChargesFilesOnly(t *testing.T) {
	root := openRoot(t)
	makeDirs(t, root, "directory-number-", 400, "a.txt")
	tree := list(t, root, ".").GetSuccess().GetDirectoryTreeRoot()
	if nodes, listed := countTree(tree); !tree.ChildrenWereProcessed || nodes != 401 || listed != 400 || tree.NumFiles != 0 {
		t.Fatalf("nodes %d, listed %d, counted %d", nodes, listed, tree.NumFiles)
	}
}

// Once the budget is spent, files are only counted, in nodes of their
// directories as on the desktop. A separate cap on directory nodes keeps a
// tree of very many directories inside a message.
func TestListBudgetLimitsDirectories(t *testing.T) {
	root := openRoot(t)
	const dirs = 3000
	makeDirs(t, root, "directory-with-a-long-name-", dirs, "f.go")
	check := func(tree *treeNode, maxNodes int) {
		t.Helper()
		nodes, listed := countTree(tree)
		wantListed := (listBudget - nameCost(root.Dir())) / len("f.go")
		if tree.ChildrenWereProcessed || nodes > maxNodes || listed != wantListed || listed+int(tree.NumFiles) != dirs ||
			tree.FullSubtreeExtensionCounts[".go"] != tree.NumFiles {
			t.Fatalf("nodes %d, listed %d, counted %d", nodes, listed, tree.NumFiles)
		}
	}
	check(list(t, root, ".").GetSuccess().DirectoryTreeRoot, dirs+1)
	// The tree that ripgrep's output would build, with room for 1000 nodes.
	b := newTreeBuilder(root.Dir())
	b.maxBytes = 1000 * (len(root.Dir()) + len("/directory-with-a-long-name-000") + nodeOverhead)
	for i := range dirs {
		b.addFile(filepath.Join(root.Dir(), fmt.Sprintf("directory-with-a-long-name-%03d", i), "f.go"))
	}
	check(b.root, 1001)
}

// Children are sorted as JavaScript's localeCompare sorts them (F6).
func TestListSortsLikeLocaleCompare(t *testing.T) {
	root := openRoot(t)
	for _, name := range []string{"README.md", "main.go", "_a", "Makefile", ".gitignore"} {
		if err := os.WriteFile(filepath.Join(root.Dir(), name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	tree := list(t, root, ".").GetSuccess().GetDirectoryTreeRoot()
	names := make([]string, 0, len(tree.ChildrenFiles))
	for _, f := range tree.ChildrenFiles {
		names = append(names, f.Name)
	}
	if got := strings.Join(names, ","); got != "_a,.gitignore,main.go,Makefile,README.md" {
		t.Fatal(got)
	}
}

// A path that cannot be listed is reported in Node's words (F13).
func TestListNodeErrors(t *testing.T) {
	root := openRoot(t)
	if err := os.WriteFile(filepath.Join(root.Dir(), "file"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := list(t, root, "file/sub").GetError().GetError()
	if want := "ENOTDIR: not a directory, stat '" + filepath.Join(root.Dir(), "file", "sub") + "'"; got != want {
		t.Fatal(got)
	}
}
