// Package proc manages the processes and temporary files of agent commands:
// a command runs in its own process group, which is killed as a whole.
package proc

import (
	"errors"
	"log"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Kill kills the process group of c, if it was started.
func Kill(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	if err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Printf("kill process group %d: %v", c.Process.Pid, err)
	}
}

// Wait reaps c, logging anything but a non-zero exit, which is reported
// through ProcessState.
func Wait(c *exec.Cmd) {
	var exitErr *exec.ExitError
	if err := c.Wait(); err != nil && !errors.As(err, &exitErr) {
		log.Printf("shell: %v", err)
	}
}

// ExitCode follows the shell convention of 128+signal for killed processes;
// a process that was never reaped counts as killed.
func ExitCode(state *os.ProcessState) int {
	if state == nil {
		return 128 + int(syscall.SIGKILL)
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return state.ExitCode()
}

// PID returns the process ID of the started c.
func PID(c *exec.Cmd) *uint32 {
	pid := uint32(c.Process.Pid) //nolint:gosec // PIDs are positive
	return &pid
}

// Timer returns a channel that fires after ms milliseconds and its stop
// function; ms <= 0 never fires.
func Timer(ms int32) (<-chan time.Time, func() bool) {
	if ms <= 0 {
		return nil, func() bool { return false }
	}
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	return t.C, t.Stop
}
