package ls

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf16"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
)

type treeNode = agentv1.LsDirectoryTreeNode

// lister builds a directory tree. Names are listed until their total length
// exceeds budget; further files are only counted per extension.
type lister struct {
	root   string
	files  *os.Root
	ignore []string // glob patterns
	budget int      // UTF-16 code units, like the desktop
}

func newNode(path string) *treeNode {
	return &treeNode{AbsPath: path, ChildrenWereProcessed: true, FullSubtreeExtensionCounts: map[string]int32{}}
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
	if strings.HasPrefix(name, ".") && strings.Count(name, ".") == 1 {
		return ""
	}
	return filepath.Ext(name)
}

// charge spends the cost of name if the budget allows. Once a name does not
// fit, nothing further is listed.
func (l *lister) charge(name string) bool {
	if cost := nameCost(name); l.budget >= cost {
		l.budget -= cost
		return true
	}
	l.budget = -1
	return false
}

// addFile lists name in node if the budget allows.
func (l *lister) addFile(node *treeNode, name string) bool {
	if !l.charge(name) {
		return false
	}
	node.ChildrenFiles = append(node.ChildrenFiles, &agentv1.LsDirectoryTreeNode_File{Name: name})
	return true
}

// countOmitted records a file that was not listed.
func countOmitted(node *treeNode, name string) {
	node.ChildrenWereProcessed = false
	node.FullSubtreeExtensionCounts[extension(name)]++
	node.NumFiles++
}

// list uses the same ripgrep enumeration as the desktop when rg is
// available. The Go fallback keeps file tools usable on hosts without
// ripgrep, but matches ignore patterns against names only.
func (l *lister) list(ctx context.Context, dir string) (*treeNode, error) {
	rg, err := exec.LookPath("rg")
	if err != nil {
		return l.walk(ctx, dir)
	}
	return l.ripgrep(ctx, rg, dir)
}

func (l *lister) ignored(name string) bool {
	return slices.ContainsFunc(l.ignore, func(pattern string) bool {
		match, err := filepath.Match(pattern, name)
		return err == nil && match
	})
}

// walk lists dir recursively without following symlinks. Directories without
// listed files are left out, and so are directories reached after the budget
// ran out: their files are only counted in the nearest listed ancestor.
func (l *lister) walk(ctx context.Context, dir string) (*treeNode, error) {
	node := newNode(filepath.Join(l.root, dir))
	if err := ctx.Err(); err != nil {
		node.ChildrenWereProcessed = false
		return node, err
	}
	entries, err := l.readDir(dir)
	if err != nil {
		return node, err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			node.ChildrenWereProcessed = false
			return node, err
		}
		name := entry.Name()
		switch {
		case l.ignored(name):
		case entry.IsDir():
			if err := l.walkChild(ctx, node, filepath.Join(dir, name)); err != nil {
				return node, err
			}
		case entry.Type()&os.ModeSymlink != 0:
		case !l.addFile(node, name):
			countOmitted(node, name)
		}
	}
	return node, nil
}

// walkChild lists a subdirectory and merges its counts into parent. Like
// file names, directory names are charged to the budget.
func (l *lister) walkChild(ctx context.Context, parent *treeNode, dir string) error {
	before := l.budget
	listed := l.charge(filepath.Base(dir))
	child, err := l.walk(ctx, dir)
	if err != nil && ctx.Err() == nil {
		// Like rg, skip a directory that cannot be read instead of failing
		// the whole listing; the agent sees it as not fully processed.
		log.Printf("ls: %v", err)
		child.ChildrenWereProcessed = false
		err = nil
	}
	switch {
	case !listed:
	case len(child.ChildrenFiles) > 0 || len(child.ChildrenDirs) > 0 || !child.ChildrenWereProcessed:
		parent.ChildrenDirs = append(parent.ChildrenDirs, child)
	default:
		l.budget = before // an empty directory is left out for free
	}
	if !child.ChildrenWereProcessed {
		parent.ChildrenWereProcessed = false
	}
	for ext, n := range child.FullSubtreeExtensionCounts {
		parent.FullSubtreeExtensionCounts[ext] += n
		parent.NumFiles += n
	}
	return err
}

func (l *lister) readDir(dir string) ([]os.DirEntry, error) {
	f, err := l.files.Open(dir)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.ReadDir(-1)
}
