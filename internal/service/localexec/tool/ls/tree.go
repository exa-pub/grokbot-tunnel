package ls

import (
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
)

type treeNode = agentv1.LsDirectoryTreeNode

const (
	// listBudget is how many UTF-16 code units the root path and the listed
	// file names may take.
	listBudget = 2500
	// maxTreeBytes bounds the directory nodes of a tree by their paths plus
	// nodeOverhead each, so that a tree of many directories stays inside a
	// message; the desktop creates directory nodes without limit.
	maxTreeBytes = 2 << 20
	nodeOverhead = 64
)

func newNode(path string, processed bool) *treeNode {
	return &treeNode{AbsPath: path, ChildrenWereProcessed: processed, FullSubtreeExtensionCounts: map[string]int32{}}
}

// nameCost is the length of name in UTF-16 code units.
func nameCost(name string) int {
	n := 0
	for _, r := range name {
		n += utf16.RuneLen(r)
	}
	return n
}

// extension is the key for extension counts. Like Node's path.extname, a
// dotfile such as ".bashrc" has none.
func extension(name string) string {
	if i := strings.LastIndexByte(name, '.'); i > 0 && name != ".." {
		return name[i:]
	}
	return ""
}

// treeBuilder assembles a tree from file paths as the desktop's
// addPathToTree does: the root path and the names of listed files are
// charged to the budget, directories are free. Once a file does not fit,
// every further file is only counted per extension in its directory and
// their ancestors; at that moment the ancestors of that file are marked as
// not fully processed, and directories created later are too.
type treeBuilder struct {
	root      *treeNode
	nodes     map[string]*treeNode // by absolute path
	used      int
	exceeded  bool
	treeBytes int
	maxBytes  int
	// The nodes the last omitted file was counted in, from its directory up.
	overflowDir   string
	overflowChain []*treeNode
}

func newTreeBuilder(root string) *treeBuilder {
	b := &treeBuilder{root: newNode(root, true), used: nameCost(root), maxBytes: maxTreeBytes}
	b.nodes = map[string]*treeNode{root: b.root}
	return b
}

func (b *treeBuilder) under(path string) bool { return strings.HasPrefix(path, b.root.AbsPath) }

// ensureDir creates the nodes of dir and its ancestors below the root, while
// the tree has room for them (ensureDirectoryPath).
func (b *treeBuilder) ensureDir(dir string) {
	if b.nodes[dir] != nil || dir == b.root.AbsPath {
		return
	}
	parent := filepath.Dir(dir)
	if parent != dir && b.under(parent) {
		b.ensureDir(parent)
	}
	if b.treeBytes+len(dir)+nodeOverhead > b.maxBytes {
		return
	}
	b.treeBytes += len(dir) + nodeOverhead
	n := newNode(dir, !b.exceeded)
	b.nodes[dir] = n
	if p := b.nodes[parent]; p != nil {
		p.ChildrenDirs = append(p.ChildrenDirs, n)
	}
}

// ancestors returns the existing nodes from dir up to the root.
func (b *treeBuilder) ancestors(dir string) []*treeNode {
	var chain []*treeNode
	for p := dir; b.under(p); {
		if n := b.nodes[p]; n != nil {
			chain = append(chain, n)
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	return chain
}

// addFile lists the file at path or, once the budget is spent, counts it.
func (b *treeBuilder) addFile(path string) {
	name, dir := filepath.Base(path), filepath.Dir(path)
	b.ensureDir(dir)
	node := b.nodes[dir]
	if node == nil && !b.under(dir) {
		return
	}
	if cost := nameCost(name); node != nil && !b.exceeded && b.used+cost <= listBudget {
		node.ChildrenFiles = append(node.ChildrenFiles, &agentv1.LsDirectoryTreeNode_File{Name: name})
		b.used += cost
		return
	}
	if node == nil {
		// No room for the directory: count the file in its nearest ancestor.
		b.ancestors(dir)[0].ChildrenWereProcessed = false
	} else if !b.exceeded {
		b.exceeded = true
		for _, n := range b.ancestors(dir) {
			n.ChildrenWereProcessed = false
		}
	}
	if b.overflowDir != dir || b.overflowChain == nil {
		b.overflowDir, b.overflowChain = dir, b.ancestors(dir)
	}
	ext := extension(name)
	for _, n := range b.overflowChain {
		n.FullSubtreeExtensionCounts[ext]++
		n.NumFiles++
	}
}

// markUnreadable records a directory that could not be listed.
func (b *treeBuilder) markUnreadable(dir string) {
	b.ensureDir(dir)
	for _, n := range b.ancestors(dir) {
		n.ChildrenWereProcessed = false
	}
}

// sort orders the children of every node by name as JavaScript's
// localeCompare does in the desktop's sortTreeChildren.
func (b *treeBuilder) sort() {
	c := collate.New(language.Und)
	var visit func(*treeNode)
	visit = func(n *treeNode) {
		slices.SortStableFunc(n.ChildrenDirs, func(x, y *treeNode) int {
			return c.CompareString(filepath.Base(x.AbsPath), filepath.Base(y.AbsPath))
		})
		slices.SortStableFunc(n.ChildrenFiles, func(x, y *agentv1.LsDirectoryTreeNode_File) int {
			return c.CompareString(x.Name, y.Name)
		})
		for _, child := range n.ChildrenDirs {
			visit(child)
		}
	}
	visit(b.root)
}

// validUTF8 replaces invalid UTF-8 in the names and paths of tree, which
// protojson cannot encode. It runs on the finished tree, since the builder
// looks nodes up by their raw paths.
func validUTF8(tree *treeNode) {
	if tree == nil {
		return
	}
	tree.AbsPath = strings.ToValidUTF8(tree.AbsPath, "\uFFFD")
	for _, f := range tree.ChildrenFiles {
		f.Name = strings.ToValidUTF8(f.Name, "\uFFFD")
	}
	for ext, n := range tree.FullSubtreeExtensionCounts {
		if valid := strings.ToValidUTF8(ext, "\uFFFD"); valid != ext {
			delete(tree.FullSubtreeExtensionCounts, ext)
			tree.FullSubtreeExtensionCounts[valid] += n
		}
	}
	for _, child := range tree.ChildrenDirs {
		validUTF8(child)
	}
}
