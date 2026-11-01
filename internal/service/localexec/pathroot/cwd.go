package pathroot

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/protobuf/proto"
)

// Cwd resolves a shell working directory like the desktop: "~" and file://
// URLs are expanded, a relative path is taken from the root, and any
// existing directory is accepted, inside the root or not; the shell is not
// confined. Otherwise the command runs in the root, with a warning for the
// agent. An empty path is the root.
func (r *Root) Cwd(path string) (dir, warning string) {
	p := r.expand(path)
	if p == "" {
		return r.dir, ""
	}
	if isDir(p) {
		return p, ""
	}
	return r.dir, fmt.Sprintf("working directory %s does not exist on this machine; running in %s instead\n", path, r.dir)
}

// IsDir reports whether dir, expanded like Cwd, is an existing directory.
func (r *Root) IsDir(dir string) bool {
	p := r.expand(dir)
	return p != "" && isDir(p)
}

// CwdState describes a shell working directory for a failed request, like
// the desktop: "exists" or "missing"; nil if it cannot be determined.
func (r *Root) CwdState(path string) *string {
	p := r.expand(path)
	if p == "" {
		p = r.dir
	}
	st, err := os.Stat(p)
	switch {
	case err == nil && st.IsDir():
		return proto.String("exists")
	case err == nil, errors.Is(err, os.ErrNotExist), errors.Is(err, os.ErrInvalid):
		return proto.String("missing")
	}
	return nil
}

// expand trims path, expands "~" and file:// URLs and makes a relative path
// absolute below the root; empty stays empty.
func (r *Root) expand(path string) string {
	p := strings.TrimSpace(path)
	switch {
	case p == "":
		return ""
	case p == "~" || strings.HasPrefix(p, "~/"):
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	case strings.HasPrefix(p, "file://"):
		if u, err := url.Parse(p); err == nil {
			p = u.Path
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.dir, p)
	}
	return filepath.Clean(p)
}

func isDir(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
