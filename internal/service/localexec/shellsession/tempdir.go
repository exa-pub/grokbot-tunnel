package shellsession

import (
	"errors"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
)

const (
	// tempPrefix starts the name of a session's private directory in the
	// system's temporary directory: grok-shell-<pid>-<random>.
	tempPrefix = "grok-shell-"
	// legacyPrefix starts the fallback snapshots that versions without the
	// private directory left in the temporary directory.
	legacyPrefix = "grok-shell-state-"
)

// tempDir creates the private directory of a session's files, mode 0700, in
// the system's temporary directory, after removing the ones that processes no
// longer running left there.
func tempDir() (string, error) {
	tmp := os.TempDir()
	removeStale(tmp)
	return os.MkdirTemp(tmp, tempPrefix+strconv.Itoa(os.Getpid())+"-")
}

// removeStale removes the user's private directories in tmp whose process no
// longer runs, and the user's fallback snapshots of older versions that no
// process has open. A process killed before Close leaves its directory
// behind, and the EXIT trap of a shell that outlives it writes the whole
// environment there.
func removeStale(tmp string) {
	entries, err := os.ReadDir(tmp)
	if err != nil {
		log.Printf("shell temporary files: %v", err)
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(tmp, name)
		switch {
		case entry.Type().IsRegular() && strings.HasPrefix(name, legacyPrefix):
			if ownFile(path) {
				removeUnused(path)
			}
		case entry.IsDir() && staleDir(name) && ownFile(path):
			if err := os.RemoveAll(path); err != nil {
				log.Printf("remove stale shell files: %v", err)
			}
		}
	}
}

// removeUnused removes the file at path unless a process has it open, as an
// older version keeps its fallback snapshot open while the command runs: the
// kernel grants a write lease only on a file that nobody else has open. A
// file whose lease cannot be taken for any reason is kept.
func removeUnused(path string) {
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return
	}
	defer f.Close()
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_SETLEASE, syscall.F_WRLCK); errno != 0 {
		return
	}
	proc.RemoveLogged(path)
}

// staleDir reports whether name is a private directory of a process that no
// longer runs. A directory of this process belongs to another session.
func staleDir(name string) bool {
	rest, ok := strings.CutPrefix(name, tempPrefix)
	if !ok {
		return false
	}
	pidText, _, ok := strings.Cut(rest, "-")
	if !ok {
		return false
	}
	pid, err := strconv.Atoi(pidText)
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return false
	}
	// EPERM: the process runs as another user.
	err = syscall.Kill(pid, 0)
	return errors.Is(err, syscall.ESRCH)
}

// ownFile reports whether path, not followed if a symlink, belongs to the
// current user.
func ownFile(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	return ok && int64(st.Uid) == int64(os.Getuid())
}
