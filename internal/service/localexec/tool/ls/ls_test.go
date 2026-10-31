package ls

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
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

// Once the budget is spent, directories are no longer listed, only counted:
// a tree of many directories stays far below the message limit.
func TestListBudgetLimitsDirectories(t *testing.T) {
	root := openRoot(t)
	const dirs = 3000
	for i := range dirs {
		dir := filepath.Join(root.Dir(), "directory-with-a-long-name-"+strconv.Itoa(i))
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "f.go"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	check := func(tree *treeNode) {
		t.Helper()
		listed, nodes := 0, 0
		var visit func(*treeNode)
		visit = func(n *treeNode) {
			nodes++
			listed += len(n.ChildrenFiles)
			for _, child := range n.ChildrenDirs {
				visit(child)
			}
		}
		visit(tree)
		if tree.ChildrenWereProcessed || nodes > listBudget/20 || listed+int(tree.NumFiles) != dirs || tree.FullSubtreeExtensionCounts[".go"] != tree.NumFiles {
			t.Fatalf("nodes %d, listed %d, counted %d", nodes, listed, tree.NumFiles)
		}
	}
	check(list(t, root, ".").GetSuccess().DirectoryTreeRoot)
	// The tree that ripgrep's output would build.
	l := &lister{root: root.Dir(), files: root.FS(), budget: listBudget}
	b := &treeBuilder{lister: l, root: newNode(root.Dir())}
	b.nodes = map[string]*treeNode{root.Dir(): b.root}
	for i := range dirs {
		b.add("directory-with-a-long-name-" + strconv.Itoa(i) + "/f.go")
	}
	check(b.root)
}
