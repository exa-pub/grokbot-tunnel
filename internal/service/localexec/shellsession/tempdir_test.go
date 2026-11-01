package shellsession

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
)

// tempEntries returns the names in dir.
func tempEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// locked reports whether a session holds path locked.
func locked(t *testing.T, path string) bool {
	t.Helper()
	f, err := lockDir(path, syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}
	proc.CloseLogged(f)
	return false
}

// S3-1: a private directory removed under a live session, as systemd-tmpfiles
// does with an idle one, is created again, once, by the commands that run in
// parallel next, with a new lock; Close removes it and releases the lock.
func TestTempDirRecreated(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	d, err := newTempDir()
	if err != nil {
		t.Fatal(err)
	}
	old := d.path
	if !strings.HasPrefix(filepath.Base(old), tempPrefix) || !locked(t, old) {
		t.Fatal(old)
	}
	if err := os.RemoveAll(old); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range cap(errs) {
		wg.Go(func() {
			f, err := d.file(func(dir string) (*os.File, error) { return proc.TempFile(dir, "state-", "x") })
			if err == nil {
				proc.CloseLogged(f)
			}
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	names := tempEntries(t, tmp)
	if len(names) != 1 || filepath.Join(tmp, names[0]) == old || filepath.Join(tmp, names[0]) != d.path {
		t.Fatal(names, old, d.path)
	}
	if !locked(t, d.path) {
		t.Fatal("new directory not locked")
	}
	if err := d.close(); err != nil {
		t.Fatal(err)
	}
	if names := tempEntries(t, tmp); len(names) != 0 {
		t.Fatal(names)
	}
	// A closed session creates no directory.
	if _, err := d.file(func(dir string) (*os.File, error) { return os.CreateTemp(dir, "x-") }); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if names := tempEntries(t, tmp); len(names) != 0 {
		t.Fatal(names)
	}
}

// S3-1: removeStale keeps the private directories that a session holds
// locked, whatever pid their name has, and those just created, which their
// session is about to lock, and removes the others, also of a pid
// that runs, as one in another pid namespace would. Only the user's own
// directories of the private name are considered, never followed through a
// symlink.
func TestRemoveStaleByLock(t *testing.T) {
	tmp := t.TempDir()
	mkdir := func(name string) string {
		path := filepath.Join(tmp, name)
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "fallback"), []byte("GROK_SECRET=hunter2"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-2 * staleAge)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		return path
	}
	mkdir("grok-shell-3-new")
	young := time.Now()
	if err := os.Chtimes(filepath.Join(tmp, "grok-shell-3-new"), young, young); err != nil {
		t.Fatal(err)
	}
	held := mkdir("grok-shell-1-held")
	holder, err := lockDir(held, syscall.LOCK_SH)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	mkdir("grok-shell-1-free")
	mkdir("grok-shell-" + strconv.Itoa(os.Getpid()) + "-free")
	mkdir("grok-shell-other")
	target := mkdir("target")
	if err := os.Symlink(target, filepath.Join(tmp, "grok-shell-2-link")); err != nil {
		t.Fatal(err)
	}
	removeStale(tmp)
	got := strings.Join(tempEntries(t, tmp), " ")
	if got != "grok-shell-1-held grok-shell-2-link grok-shell-3-new grok-shell-other target" {
		t.Fatal(got)
	}
	if _, err := os.Stat(filepath.Join(target, "fallback")); err != nil {
		t.Fatal(err)
	}
	// Released, the directory is stale.
	proc.CloseLogged(holder)
	removeStale(tmp)
	if got := strings.Join(tempEntries(t, tmp), " "); got != "grok-shell-2-link grok-shell-3-new grok-shell-other target" {
		t.Fatal(got)
	}
}

// S3-1: a directory that removeStale of another process may remove between
// its creation and its lock is never handed out: once create returns, the
// directory is in place and removeStale keeps it.
func TestTempDirCreateRacesRemoveStale(t *testing.T) {
	tmp := t.TempDir()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				removeStale(tmp)
			}
		}
	})
	defer func() {
		close(stop)
		wg.Wait()
	}()
	for range 200 {
		d := &tempDir{}
		if err := d.create(tmp); err != nil {
			t.Fatal(err)
		}
		removeStale(tmp)
		if !sameDir(d.path, d.lock) {
			t.Fatal(d.path, "removed")
		}
		if err := d.close(); err != nil {
			t.Fatal(err)
		}
	}
}

// S3-1: a temporary directory that cannot be used fails the session at once,
// naming it as the shell's.
func TestTempDirUnusable(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	t.Setenv("TMPDIR", missing)
	_, err := newTempDir()
	if err == nil || !strings.Contains(err.Error(), "create the shell's temporary directory in "+missing+" (set TMPDIR to a writable directory)") {
		t.Fatal(err)
	}
}
