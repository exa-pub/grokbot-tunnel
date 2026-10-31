package pathroot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"google.golang.org/protobuf/proto"
)

// Cwd resolves a shell working directory. A missing directory falls back to
// the root with a warning for the agent instead of failing the command.
// ≠ desktop: a directory outside the root is refused; the desktop accepts any
// existing directory (rnd-v2 Q2).
func (r *Root) Cwd(path string) (dir, warning string, err error) {
	rel, err := r.Relative(path)
	if err != nil {
		return "", "", err
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(r.dir, rel))
	if errors.Is(err, os.ErrNotExist) {
		return r.dir, fmt.Sprintf("working directory %s does not exist on this machine; running in %s instead\n", path, r.dir), nil
	}
	if err != nil {
		return "", "", err
	}
	if _, err = r.Relative(resolved); err != nil {
		return "", "", err
	}
	st, err := os.Stat(resolved)
	if err != nil {
		return "", "", err
	}
	if !st.IsDir() {
		return "", "", errors.New("working directory is not a directory")
	}
	return resolved, "", nil
}

// IsDir reports whether dir is a directory below the root.
func (r *Root) IsDir(dir string) bool {
	rel, err := r.Relative(dir)
	if err != nil {
		return false
	}
	info, err := r.fs.Stat(rel)
	return err == nil && info.IsDir()
}

// CwdState describes a shell working directory for a failed request:
// "exists" or "missing"; nil if it cannot be determined.
func (r *Root) CwdState(path string) *string {
	rel, err := r.Relative(path)
	if err != nil {
		return nil
	}
	st, err := r.fs.Stat(rel)
	switch {
	case err == nil && st.IsDir():
		return proto.String("exists")
	case errors.Is(err, os.ErrNotExist):
		return proto.String("missing")
	}
	return nil
}
