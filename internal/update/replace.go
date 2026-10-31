package update

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/atomicfile"
)

const verifyTimeout = 10 * time.Second

// Executable returns the path of the running binary with symlinks resolved,
// so that an update replaces the file rather than a link to it.
func Executable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// Replace atomically replaces the binary at path with bin after checking that
// bin runs and reports version. A failure leaves path untouched.
func Replace(ctx context.Context, path string, bin []byte, version string) error {
	// The running process keeps the old file open; renaming over it is safe.
	return atomicfile.Write(path, bin, atomicfile.Options{
		Mode:   0o755, // executables must be executable
		Prefix: "." + filepath.Base(path) + ".update-",
		Check:  func(tmp string) error { return verify(ctx, tmp, version) },
	})
}

// verify runs bin --version, which catches a wrong architecture, a noexec
// file system or a broken download before they replace a working binary.
func verify(ctx context.Context, bin, version string) error {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	var out []byte
	var err error
	// A concurrent fork elsewhere may briefly hold the new file open for
	// writing, which makes executing it fail with ETXTBSY.
	for range 5 {
		out, err = exec.CommandContext(ctx, bin, "--version").Output() //nolint:gosec // our own download
		if !errors.Is(err, syscall.ETXTBSY) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		return fmt.Errorf("the downloaded binary does not run on this system: %w", err)
	}
	if !strings.HasPrefix(string(out), "grokbot-tunnel "+version+" ") {
		return fmt.Errorf("the downloaded binary reports %q, want version %s", strings.TrimSpace(string(out)), version)
	}
	return nil
}
