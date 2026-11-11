package proc_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
)

func TestTimer(t *testing.T) {
	never, stop := proc.Timer(0)
	assert.Nil(t, never)
	assert.False(t, stop())
	fires, stop := proc.Timer(1)
	<-fires
	assert.False(t, stop())
}

func TestTempFileIsUnlinked(t *testing.T) {
	dir := t.TempDir()
	f, err := proc.TempFile(dir, "proc-test-", "content")
	require.NoError(t, err)
	defer proc.CloseLogged(f)
	assert.Equal(t, dir, filepath.Dir(f.Name()))
	_, err = os.Stat(f.Name())
	require.ErrorIs(t, err, os.ErrNotExist)
	b, err := io.ReadAll(f) // positioned at the start
	require.NoError(t, err)
	assert.Equal(t, "content", string(b))
	proc.RemoveLogged(f.Name()) // already gone: not logged as an error
}

// Kill ends the whole process group, including children of the shell, with
// SIGTERM, and a second later with SIGKILL what ignored it, like the desktop.
func TestKillProcessGroup(t *testing.T) {
	for _, tc := range []struct {
		script string
		signal syscall.Signal
		after  time.Duration
	}{
		{"sleep 30 & sleep 30", syscall.SIGTERM, 0},
		{"trap '' TERM; sleep 30 & while :; do sleep 0.05; done", syscall.SIGKILL, time.Second},
	} {
		c := exec.CommandContext(context.Background(), "/bin/sh", "-c", tc.script)
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		require.NoError(t, c.Start())
		time.Sleep(50 * time.Millisecond) // let the shell set its trap
		start := time.Now()
		proc.Kill(c)
		done := make(chan struct{})
		go func() { proc.Wait(c); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("process group not killed")
		}
		assert.GreaterOrEqual(t, time.Since(start), tc.after, tc.script)
		ws, ok := c.ProcessState.Sys().(syscall.WaitStatus)
		require.True(t, ok)
		assert.True(t, ws.Signaled(), tc.script)
		assert.Equal(t, tc.signal, ws.Signal(), tc.script)
		assert.Equal(t, uint32(c.Process.Pid), *proc.PID(c)) //nolint:gosec // PIDs are positive
		// Orphaned children are reaped by init a moment later.
		assert.Eventually(t, func() bool { return errors.Is(syscall.Kill(-c.Process.Pid, 0), syscall.ESRCH) },
			5*time.Second, 10*time.Millisecond, "process group survived")
	}
	proc.Kill(&exec.Cmd{}) // not started: no-op
}

// gone reports whether pid has exited: it no longer exists or is a zombie
// waiting for init.
func gone(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true
	}
	_, after, _ := strings.Cut(string(b), ") ")
	return strings.HasPrefix(after, "Z")
}

// A shell killed by a terminating signal takes its process group with it;
// one that exits on its own or by another signal does not (Np).
func TestForward(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc")
	}
	for _, tc := range []struct {
		script string
		killed bool
	}{
		{"sleep 30 & echo $!; kill -KILL $$", true},
		{"sleep 30 & echo $!; kill -HUP $$", true},
		{"sleep 30 & echo $!; kill -USR1 $$", false},
		{"sleep 30 & echo $!; exit 3", false},
	} {
		c := exec.CommandContext(context.Background(), "/bin/sh", "-c", tc.script)
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		out, err := c.StdoutPipe()
		require.NoError(t, err)
		require.NoError(t, c.Start())
		var pid int
		_, err = fmt.Fscan(out, &pid)
		require.NoError(t, err, tc.script)
		proc.Wait(c)
		proc.Forward(c)
		if tc.killed {
			assert.Eventually(t, func() bool { return gone(pid) }, 5*time.Second, 10*time.Millisecond, tc.script)
		} else {
			time.Sleep(100 * time.Millisecond)
			assert.False(t, gone(pid), tc.script)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	proc.Forward(&exec.Cmd{}) // not started: no-op
}
