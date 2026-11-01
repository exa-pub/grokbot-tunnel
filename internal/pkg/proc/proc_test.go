package proc_test

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
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
	f, err := proc.TempFile("proc-test-", "content")
	require.NoError(t, err)
	defer proc.CloseLogged(f)
	_, err = os.Stat(f.Name())
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = f.Seek(0, io.SeekStart)
	require.NoError(t, err)
	b, err := io.ReadAll(f)
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
