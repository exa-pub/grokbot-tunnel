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

// Kill ends the whole process group, including children of the shell.
func TestKillProcessGroup(t *testing.T) {
	c := exec.CommandContext(context.Background(), "/bin/sh", "-c", "sleep 30 & sleep 30")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, c.Start())
	proc.Kill(c)
	done := make(chan struct{})
	go func() { proc.Wait(c); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("process group not killed")
	}
	assert.Equal(t, 128+int(syscall.SIGKILL), proc.ExitCode(c.ProcessState))
	assert.Equal(t, uint32(c.Process.Pid), *proc.PID(c)) //nolint:gosec // PIDs are positive
	// Orphaned children are reaped by init a moment later.
	assert.Eventually(t, func() bool { return errors.Is(syscall.Kill(-c.Process.Pid, 0), syscall.ESRCH) },
		5*time.Second, 10*time.Millisecond, "process group survived")
	proc.Kill(&exec.Cmd{}) // not started: no-op
}

func TestExitCode(t *testing.T) {
	c := exec.CommandContext(context.Background(), "/bin/sh", "-c", "exit 3")
	require.NoError(t, c.Start())
	proc.Wait(c)
	assert.Equal(t, 3, proc.ExitCode(c.ProcessState))
	assert.Equal(t, 137, proc.ExitCode(nil))
}

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
