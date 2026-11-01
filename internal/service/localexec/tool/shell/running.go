package shell

import (
	"context"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/shellsession"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
)

// runningShell is a started foreground command.
//
// Its stdout and stderr are pipes read by drainers that outlive the shell:
// background jobs the command started may keep writing to them, and closing
// the pipes would kill those jobs with SIGPIPE. Once the shell has exited and
// the drainers have hit EOF or waitDelay has passed, the output is finished:
// nothing more reaches the agent or the terminal file, but the drainers keep
// reading until the last writer is gone.
//
// Like the desktop's io(), which stops listening for the abort once the shell
// exits, an abort after the shell has exited kills nothing: the background
// jobs left in its process group keep running.
type runningShell struct {
	ex       *Executor
	args     *agentv1.ShellArgs
	cwd      string
	state    shellsession.State // the state the command started with
	cmd      *exec.Cmd
	out      *shellOutput
	stdout   *streamWriter
	stderr   *streamWriter
	drained  sync.WaitGroup // the drainers have hit EOF
	done     chan struct{}  // closed once the process has exited and its output is finished
	started  time.Time
	snapshot *os.File    // receives the state on exit
	fallback *os.File    // receives the state from the EXIT trap; linked until the shell exits
	killed   atomic.Bool // a detached command was killed by the hard timeout

	exitMu sync.Mutex // guards gone
	gone   bool       // the shell has exited and been reaped
}

// start launches the command with its output pipes and their drainers.
func (sh *runningShell) start() error {
	stdout, stdoutW, err := os.Pipe()
	if err != nil {
		return err
	}
	stderr, stderrW, err := os.Pipe()
	if err != nil {
		proc.CloseLogged(stdout)
		proc.CloseLogged(stdoutW)
		return err
	}
	sh.cmd.Stdout, sh.cmd.Stderr = stdoutW, stderrW
	err = sh.cmd.Start()
	// Only the command keeps the write ends, so the drainers see EOF once it
	// and its children are gone.
	proc.CloseLogged(stdoutW)
	proc.CloseLogged(stderrW)
	if err != nil {
		proc.CloseLogged(stdout)
		proc.CloseLogged(stderr)
		return err
	}
	sh.drained.Add(2)
	go sh.drain(stdout, sh.stdout)
	go sh.drain(stderr, sh.stderr)
	return nil
}

// drain copies r to w until EOF; w discards what arrives after the output is
// finished.
func (sh *runningShell) drain(r *os.File, w *streamWriter) {
	defer sh.drained.Done()
	defer proc.CloseLogged(r)
	if _, err := io.Copy(w, r); err != nil {
		log.Printf("shell output: %v", err)
	}
}

// reap waits for the process and its output and finalizes the output.
func (sh *runningShell) reap() {
	proc.Wait(sh.cmd)
	sh.exitMu.Lock()
	sh.gone = true
	sh.exitMu.Unlock()
	sh.ex.session.Exited(sh.cmd, sh.snapshot, sh.fallback)
	proc.RemoveLogged(sh.fallback.Name())
	drained := make(chan struct{})
	go func() {
		sh.drained.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(waitDelay):
	}
	sh.stdout.flush()
	sh.stderr.flush()
	if f, shellID, full := sh.out.finish(); f != nil {
		// A detached command writes its state once it ends on its own, like
		// the desktop, before the footer: the command that ends last wins.
		killed := sh.killed.Load()
		if !killed {
			sh.ex.session.Commit(sh.snapshot, sh.fallback, sh.state)
		}
		if _, note := full.close(); note != "" {
			f.WriteText(note)
		}
		if err := f.Exit(terminalfile.Code(sh.cmd.ProcessState), killed); err != nil {
			log.Printf("terminal %d: %v", shellID, err)
		}
		sh.ex.background.Release(shellID)
	}
	close(sh.done)
}

// kill kills the command's process group unless the shell has exited; if it
// does, marked, if any, is set first.
func (sh *runningShell) kill(marked *atomic.Bool) {
	sh.exitMu.Lock()
	defer sh.exitMu.Unlock()
	if sh.gone {
		return
	}
	if marked != nil {
		marked.Store(true)
	}
	proc.Kill(sh.cmd)
}

// exited reports whether the command has exited and its output is finished.
func (sh *runningShell) exited() bool {
	select {
	case <-sh.done:
		return true
	default:
		return false
	}
}

func (sh *runningShell) wait(ctx context.Context) error {
	timeout := softTimeout(sh.args)
	soft := time.NewTimer(time.Duration(max(timeout, 0)) * time.Millisecond) // 0 fires at once
	defer soft.Stop()
	hard, stopHard := proc.Timer(sh.args.GetHardTimeout())
	defer stopHard()
	reason := agentv1.ShellAbortReason_SHELL_ABORT_REASON_UNSPECIFIED
	select {
	case <-sh.done:
	case <-ctx.Done():
		if sh.canBackground() && !sh.exited() {
			// Like the desktop, the stream ends at once with an exit of its
			// own; the command is killed meanwhile.
			sh.abandon()
			if err := sh.out.sendErr(); err != nil {
				return err
			}
			return sh.out.send(shellMessage(&agentv1.ShellStream{Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{
				Code: math.MaxUint32, Cwd: sh.ex.session.CurrentCwd(), Aborted: true,
				AbortReason: agentv1.ShellAbortReason_SHELL_ABORT_REASON_USER_ABORT.Enum(),
			}}}))
		}
		reason = agentv1.ShellAbortReason_SHELL_ABORT_REASON_USER_ABORT
	case <-hard:
		if sh.canBackground() && !sh.exited() {
			// Like the desktop, the stream ends at once without an exit.
			sh.abandon()
			return sh.out.sendErr()
		}
		reason = agentv1.ShellAbortReason_SHELL_ABORT_REASON_TIMEOUT
	case <-soft.C:
		if sh.canBackground() && !sh.exited() {
			stopHard()
			return sh.detach(timeout)
		}
		reason = agentv1.ShellAbortReason_SHELL_ABORT_REASON_TIMEOUT
	}
	// select picks at random when several are ready: a command that has
	// already finished is reported as such.
	if sh.exited() {
		reason = agentv1.ShellAbortReason_SHELL_ABORT_REASON_UNSPECIFIED
	}
	return sh.finish(reason)
}

// abandon stops the output from reaching the agent and kills the command
// unless its shell has exited; its state is not kept.
func (sh *runningShell) abandon() {
	_, _, full := sh.out.finish()
	full.release()
	sh.kill(nil)
}

// detach leaves the command running in the background, like the desktop's
// adopt: its terminal file is created now, with the output so far, and the
// agent learns the shell ID and how long the command ran in the foreground.
// The hard timeout, counted from the start, still applies. A command that
// exits meanwhile is reported as exited instead. If the terminal file cannot
// be created, the command is killed before the error is reported.
func (sh *runningShell) detach(waited int32) error {
	shellID, f, err := sh.ex.background.Terminal()
	if err != nil {
		sh.abandon()
		<-sh.done
		return err
	}
	detached, err := sh.out.detach(shellID, func(held string) (*terminalfile.File, error) {
		cmd := terminalfile.Command{PID: sh.cmd.Process.Pid, Cwd: sh.cwd, Command: sh.args.Command,
			Title: sh.args.GetDescription(), Started: sh.started}
		return terminalfile.Start(f, cmd, held)
	})
	if !detached {
		proc.CloseLogged(f)
		if err := sh.ex.background.Remove(shellID); err != nil {
			log.Printf("terminal %d: %v", shellID, err)
		}
	}
	if err != nil {
		sh.abandon()
		<-sh.done
		return err
	}
	if !detached {
		<-sh.done
		return sh.finish(agentv1.ShellAbortReason_SHELL_ABORT_REASON_UNSPECIFIED)
	}
	if hard := sh.args.GetHardTimeout(); hard > 0 {
		remaining := time.Duration(hard)*time.Millisecond - time.Since(sh.started)
		go func() {
			select {
			case <-sh.done:
			case <-time.After(max(remaining, time.Millisecond)):
				sh.kill(&sh.killed)
			}
		}()
	}
	return sh.out.send(shellMessage(&agentv1.ShellStream{Event: &agentv1.ShellStream_Backgrounded{
		Backgrounded: &agentv1.ShellStreamBackgrounded{
			ShellId:          shellID,
			Command:          sh.args.Command,
			WorkingDirectory: sh.cwd,
			Pid:              proc.PID(sh.cmd),
			MsToWait:         &waited,
			Reason:           agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT.Enum(),
		},
	}}))
}

// finish kills the command if it was aborted while its shell still ran and
// reports its exit with the session's cwd once its output is finished, as the
// desktop does: a command that ended on its own has written the state it left
// first. An aborted command's state is discarded.
func (sh *runningShell) finish(reason agentv1.ShellAbortReason) error {
	aborted := reason != agentv1.ShellAbortReason_SHELL_ABORT_REASON_UNSPECIFIED
	if aborted {
		sh.kill(nil)
		<-sh.done
	}
	if err := sh.out.sendErr(); err != nil {
		return err
	}
	if !aborted {
		sh.ex.session.Commit(sh.snapshot, sh.fallback, sh.state)
	}
	location, note := sh.out.full.close()
	if note != "" {
		if err := sh.out.send(shellMessage(stderrEvent(note))); err != nil {
			return err
		}
	}
	exit := &agentv1.ShellStreamExit{
		Code:                 exitCode(sh.cmd.ProcessState),
		Cwd:                  sh.ex.session.CurrentCwd(),
		OutputLocation:       location,
		Aborted:              aborted,
		LocalExecutionTimeMs: elapsedMs(sh.started),
	}
	if aborted {
		exit.AbortReason = &reason // only on abort, as on the desktop
	}
	return sh.out.send(shellMessage(&agentv1.ShellStream{Event: &agentv1.ShellStream_Exit{Exit: exit}}))
}

func (sh *runningShell) canBackground() bool {
	return sh.args.TimeoutBehavior == agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND
}

// exitCode is the exit code the desktop reports: Node's code, which is null
// for a process killed by a signal, as an unsigned (code ?? -1) >>> 0.
func exitCode(state *os.ProcessState) uint32 {
	if state == nil {
		return math.MaxUint32
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return math.MaxUint32
	}
	return uint32(state.ExitCode()) //nolint:gosec // -1 only for unfinished processes, as above
}
