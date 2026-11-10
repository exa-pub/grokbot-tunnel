// Package subject starts the clients the comparison tests compare:
// grokbot-tunnel and the desktop client's local-exec daemon. Both are pointed
// at the fake backend and given the same home, root and environment.
package subject

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Env is what a client is started with.
type Env struct {
	// BackendURL is the fake backend.
	BackendURL string
	// Home is the home directory and Root the directory for files and
	// commands.
	Home, Root string
	// Shell is the user's shell.
	Shell string
	// Permission is "always", "ask" or "never".
	Permission string
	// ServerAuthoritative says the backend decides on permissions; the fake
	// backend must be set up the same way.
	ServerAuthoritative bool
	// Patches are the changes to the desktop's bundle the scenario asks
	// for; grokbot-tunnel ignores them.
	Patches []string
	// Run marks the processes of this run so that Stop finds those the
	// client leaves behind.
	Run string
}

// marker is the environment variable that carries Env.Run.
const marker = "GROKBOT_COMPARE_RUN"

// Subject is a client under comparison.
type Subject interface {
	// Name identifies the client in transcripts and reports.
	Name() string
	// Start starts the client and returns once its process runs; the caller
	// waits for its watch stream.
	Start(t testing.TB, env Env) *Running
}

// Running is a started client.
type Running struct {
	cmd    *exec.Cmd
	log    *lockedBuffer
	exited chan struct{}
	env    Env
}

// environ is the environment both clients run in: no more than a login
// gives, so that neither sees a variable the other does not.
func (e Env) environ(extra ...string) []string {
	return append([]string{
		"HOME=" + e.Home,
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"SHELL=" + e.Shell,
		"USER=tester",
		"LOGNAME=tester",
		"LANG=C.UTF-8",
		marker + "=" + e.Run,
	}, extra...)
}

func start(t testing.TB, env Env, cmd *exec.Cmd) *Running {
	t.Helper()
	r := &Running{cmd: cmd, log: &lockedBuffer{}, exited: make(chan struct{}), env: env}
	if cmd.Stdout == nil {
		cmd.Stdout = r.log
	}
	cmd.Stderr = r.log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", cmd.Path, err)
	}
	go func() {
		cmd.Wait() //nolint:errcheck // how a client ends is the scenario's business
		close(r.exited)
	}()
	t.Cleanup(r.Stop)
	return r
}

// Log returns what the client printed so far.
func (r *Running) Log() string { return r.log.String() }

// Exited reports whether the client's process has ended.
func (r *Running) Exited() bool {
	select {
	case <-r.exited:
		return true
	default:
		return false
	}
}

// Stop ends the client with SIGTERM, then kills it and every process it left
// behind. It may be called more than once.
func (r *Running) Stop() {
	if !r.Exited() {
		r.cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck // it may have just ended
		select {
		case <-r.exited:
		case <-time.After(10 * time.Second):
			r.cmd.Process.Kill() //nolint:errcheck // it may have just ended
			<-r.exited
		}
	}
	killMarked(r.env.Run)
}

// killMarked kills the processes whose environment carries the run marker:
// background commands and shells that outlive the client on purpose.
func killMarked(run string) {
	want := []byte(marker + "=" + run)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		environ, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err != nil {
			continue
		}
		for _, kv := range bytes.Split(environ, []byte{0}) {
			if bytes.Equal(kv, want) {
				syscall.Kill(pid, syscall.SIGKILL) //nolint:errcheck // it may have just ended
				break
			}
		}
	}
}

// run runs a command to its end and fails the test if it fails.
func run(t testing.TB, env []string, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // the client under test
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("%s: %v", name, err)
		}
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// Version returns the first line a program prints for its version flag, or
// "" if it does not run.
func Version(name string, args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // a program of the image
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}

// requireImage fails the test unless it runs in the comparison image, where
// both clients see the same programs and no network.
func requireImage(t testing.TB, want ...string) {
	t.Helper()
	got := os.Getenv("GROKBOT_COMPARE_IMAGE")
	for _, w := range want {
		if got == w {
			return
		}
	}
	t.Fatalf("this client runs only in the comparison image (%s); use make compare", strings.Join(want, " or "))
}
