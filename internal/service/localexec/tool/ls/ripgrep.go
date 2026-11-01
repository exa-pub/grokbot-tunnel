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
	"strings"
	"time"
)

// ripgrep adds the files rg would search to b. rg runs in the directory
// opened through Root, pinned by its descriptor, and does not follow
// symlinks. On timeout the tree stays as it is, like the desktop's.
func (l *lister) ripgrep(ctx context.Context, b *treeBuilder, rg, dir string) error {
	fd, err := l.files.Open(filepath.Join(l.base, dir))
	if err != nil {
		return err
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
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	scanner.Split(scanNUL)
	found := 0
	for scanner.Scan() {
		rel := filepath.Clean(scanner.Text())
		if rel != ".." && !filepath.IsAbs(rel) && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			b.addFile(filepath.Join(b.root.AbsPath, rel))
		}
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
	if err := ctx.Err(); err != nil {
		return err
	}
	if scanErr != nil {
		return scanErr
	}
	return ripgrepStatus(waitErr, found, stderr.String())
}

// ripgrepStatus interprets how rg exited after listing found paths. Like the
// desktop, the exit status only matters when rg listed nothing: it exits with
// 1 when there are no files and with 2 when some directories could not be
// read, which still leaves a usable tree. rg's stderr is quoted as it is.
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
			log.Printf("ls: ripgrep exited with %d: %s", code, strings.TrimSpace(stderr))
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
