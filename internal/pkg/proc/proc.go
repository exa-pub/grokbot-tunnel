// Package proc manages the processes and temporary files of agent commands:
// a command runs in its own process group, which is killed as a whole.
package proc

import (
	"errors"
	"log"
	"os/exec"
	"syscall"
	"time"
)

// killDelay is how long a process group has between SIGTERM and SIGKILL,
// like the desktop's.
const killDelay = time.Second

// Kill stops the process group of c, if it was started, like the desktop: a
// SIGTERM, then a SIGKILL after killDelay, whether or not the group is gone.
func Kill(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	pid := c.Process.Pid
	signalGroup(pid, syscall.SIGTERM)
	time.AfterFunc(killDelay, func() { signalGroup(pid, syscall.SIGKILL) })
}

// forwarded are the signals that, when one kills a shell, the desktop sends
// on to the rest of its process group (FTt).
var forwarded = map[syscall.Signal]bool{
	syscall.SIGTERM: true, syscall.SIGINT: true, syscall.SIGHUP: true, syscall.SIGQUIT: true, syscall.SIGKILL: true,
}

// Forward sends the signal that killed the reaped c, if it is one of those,
// to c's process group, like the desktop since 1.0.1 (Np) for a shell that
// was not aborted: the jobs a command started do not outlive a shell that
// was killed. The caller skips it for a shell it killed itself.
func Forward(c *exec.Cmd) {
	if c.Process == nil || c.ProcessState == nil {
		return
	}
	ws, ok := c.ProcessState.Sys().(syscall.WaitStatus)
	if ok && ws.Signaled() && forwarded[ws.Signal()] {
		signalGroup(c.Process.Pid, ws.Signal())
	}
}

func signalGroup(pid int, sig syscall.Signal) {
	if err := syscall.Kill(-pid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		log.Printf("signal process group %d: %v", pid, err)
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
