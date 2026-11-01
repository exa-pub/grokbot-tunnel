package ls

import (
	"context"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// lister builds a directory tree below root, the absolute path of the
// listed directory as requested; base is the same directory in files.
type lister struct {
	root   string
	files  *os.Root
	base   string
	ignore []string // glob patterns
}

// list uses the same ripgrep enumeration as the desktop when rg is
// available. The Go fallback keeps file tools usable on hosts without
// ripgrep, but matches ignore patterns against names only. Either way the
// tree is returned, partial if the listing stopped early.
func (l *lister) list(ctx context.Context, dir string) (*treeNode, error) {
	b := newTreeBuilder(filepath.Join(l.root, dir))
	var err error
	if rg, lookErr := exec.LookPath("rg"); lookErr == nil {
		err = l.ripgrep(ctx, b, rg, dir)
	} else {
		err = l.walk(ctx, b, dir, true)
	}
	b.sort()
	return b.root, err
}

func (l *lister) ignored(name string) bool {
	return slices.ContainsFunc(l.ignore, func(pattern string) bool {
		match, err := filepath.Match(pattern, name)
		return err == nil && match
	})
}

// walk adds the files below dir without following symlinks, in name order.
// Like rg, it skips a subdirectory that cannot be read instead of failing
// the whole listing; the agent sees it as not fully processed.
func (l *lister) walk(ctx context.Context, b *treeBuilder, dir string, top bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entries, err := l.readDir(dir)
	if err != nil {
		if top {
			return err
		}
		log.Printf("ls: %v", err)
		b.markUnreadable(filepath.Join(l.root, dir))
		return nil
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		switch {
		case l.ignored(name):
		case entry.IsDir():
			if err := l.walk(ctx, b, filepath.Join(dir, name), false); err != nil {
				return err
			}
		case entry.Type()&os.ModeSymlink != 0:
		default:
			b.addFile(filepath.Join(l.root, dir, name))
		}
	}
	return nil
}

func (l *lister) readDir(dir string) ([]os.DirEntry, error) {
	f, err := l.files.Open(filepath.Join(l.base, dir))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	entries, err := f.ReadDir(-1)
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, err
}
