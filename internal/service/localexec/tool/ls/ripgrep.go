package ls

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
)

// ripgrep lists the files rg would search. rg runs in the directory opened
// through Root, pinned by its descriptor, and does not follow symlinks.
func (l *lister) ripgrep(ctx context.Context, rg, dir string) (*treeNode, error) {
	tree := newNode(filepath.Join(l.root, dir))
	fd, err := l.files.Open(dir)
	if err != nil {
		return tree, err
	}
	defer fd.Close()
	args := []string{"--files", "--hidden", "--no-require-git", "--no-config", "--color=never", "--case-sensitive", "--null"}
	for _, pattern := range l.ignore {
		args = append(args, "--iglob", "!"+pattern)
	}
	cmd := exec.CommandContext(ctx, rg, args...) //nolint:gosec // rg from PATH; patterns are plain arguments
	cmd.Dir = fmt.Sprintf("/proc/self/fd/%d", fd.Fd())
	cmd.WaitDelay = time.Second // do not wait for descendants holding the pipes
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return tree, err
	}
	if err = cmd.Start(); err != nil {
		return tree, err
	}
	b := &treeBuilder{lister: l, root: tree, nodes: map[string]*treeNode{tree.AbsPath: tree}}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	scanner.Split(scanNUL)
	found := 0
	for scanner.Scan() {
		b.add(scanner.Text())
		found++
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		// Nobody reads stdout any more; do not let rg block on a full pipe.
		if err := cmd.Process.Kill(); err != nil {
			log.Printf("ls: kill ripgrep: %v", err)
		}
	}
	waitErr := cmd.Wait()
	b.sort()
	if err := ctx.Err(); err != nil {
		tree.ChildrenWereProcessed = false
		return tree, err
	}
	if scanErr != nil {
		return tree, scanErr
	}
	return tree, ripgrepStatus(waitErr, found, strings.TrimSpace(stderr.String()))
}

// ripgrepStatus interprets how rg exited after listing found paths. Like the
// desktop, the exit status only matters when rg listed nothing: it exits with
// 1 when there are no files and with 2 when some directories could not be
// read, which still leaves a usable tree.
func ripgrepStatus(waitErr error, found int, stderr string) error {
	if waitErr == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return fmt.Errorf("ripgrep failed: %w", waitErr)
	}
	code := exitErr.ExitCode()
	switch {
	case found > 0:
		if stderr != "" {
			log.Printf("ls: ripgrep exited with %d: %s", code, stderr)
		}
		return nil
	case code == 1:
		return nil
	case stderr != "":
		stderr = ": " + stderr
	}
	return fmt.Errorf("Ripgrep failed (exit %d) with no results%s", code, stderr) //nolint:staticcheck // desktop text
}

// scanNUL splits NUL-terminated records.
func scanNUL(data []byte, atEOF bool) (int, []byte, error) {
	if i := bytes.IndexByte(data, 0); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// treeBuilder assembles a tree from a flat list of relative file paths.
type treeBuilder struct {
	lister   *lister
	root     *treeNode
	nodes    map[string]*treeNode // by absolute path
	overflow bool                 // the budget ran out
}

// dir returns the node for path, creating it and its ancestors while the
// budget allows directory names; ok is false if it returns the nearest
// existing ancestor instead. Once the budget runs out no nodes are created, so
// a large tree cannot exceed the message size through directories alone.
func (b *treeBuilder) dir(path string) (_ *treeNode, ok bool) {
	if n := b.nodes[path]; n != nil {
		return n, true
	}
	parent, ok := b.dir(filepath.Dir(path))
	if !ok || b.overflow || !b.lister.charge(filepath.Base(path)) {
		b.overflow = true
		return parent, false
	}
	n := newNode(path)
	b.nodes[path] = n
	parent.ChildrenDirs = append(parent.ChildrenDirs, n)
	return n, true
}

func (b *treeBuilder) add(rel string) {
	rel = filepath.Clean(rel)
	if rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	path := filepath.Join(b.root.AbsPath, rel)
	parent, ok := b.dir(filepath.Dir(path))
	name := filepath.Base(path)
	if ok && !b.overflow && b.lister.addFile(parent, name) {
		return
	}
	b.overflow = true
	// Count the omitted file in every ancestor up to the root.
	for n := parent; n != nil; n = b.nodes[filepath.Dir(n.AbsPath)] {
		countOmitted(n, name)
		if n == b.root {
			break
		}
	}
}

// validUTF8 replaces invalid UTF-8 in the names and paths of tree, which
// protojson cannot encode. It runs on the finished tree, since the builder
// looks nodes up by their raw paths.
func validUTF8(tree *treeNode) {
	if tree == nil {
		return
	}
	tree.AbsPath = strings.ToValidUTF8(tree.AbsPath, "�")
	for _, f := range tree.ChildrenFiles {
		f.Name = strings.ToValidUTF8(f.Name, "�")
	}
	for ext, n := range tree.FullSubtreeExtensionCounts {
		if valid := strings.ToValidUTF8(ext, "�"); valid != ext {
			delete(tree.FullSubtreeExtensionCounts, ext)
			tree.FullSubtreeExtensionCounts[valid] += n
		}
	}
	for _, child := range tree.ChildrenDirs {
		validUTF8(child)
	}
}

func (b *treeBuilder) sort() {
	for _, n := range b.nodes {
		slices.SortFunc(n.ChildrenDirs, func(x, y *treeNode) int {
			return strings.Compare(filepath.Base(x.AbsPath), filepath.Base(y.AbsPath))
		})
		slices.SortFunc(n.ChildrenFiles, func(x, y *agentv1.LsDirectoryTreeNode_File) int {
			return strings.Compare(x.Name, y.Name)
		})
	}
}
