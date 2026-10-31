package shell

import (
	"context"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
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
type runningShell struct {
	ex       *Executor
	args     *agentv1.ShellArgs
	cwd      string
	cmd      *exec.Cmd
	out      *shellOutput
	stdout   *streamWriter
	stderr   *streamWriter
	drained  sync.WaitGroup      // the drainers have hit EOF
	done     chan struct{}       // closed once the process has exited and its output is finished
	shellID  uint32              // terminal file number, if the command may be backgrounded
	file     *os.File            // terminal file, if the command may be backgrounded
	statusAt int64               // offset of the status in the terminal file
	snapshot *os.File            // receives the state on exit
	fallback *os.File            // receives the state from the EXIT trap; linked until the shell exits
	final    *shellsession.State // the state the command left, once it has exited
}

// start launches the command with its output pipes and their drainers. The
// output lock is held until the terminal header is written so that the header
// precedes any output.
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
	sh.out.mu.Lock()
	err = sh.cmd.Start()
	// Only the command keeps the write ends, so the drainers see EOF once it
	// and its children are gone.
	proc.CloseLogged(stdoutW)
	proc.CloseLogged(stderrW)
	if err == nil && sh.file != nil {
		sh.statusAt, err = terminalfile.WriteHeader(sh.file, sh.cwd, sh.args.Command, sh.cmd.Process.Pid, "")
	}
	sh.out.mu.Unlock()
	if err != nil && sh.cmd.Process != nil {
		proc.Kill(sh.cmd)
		proc.Wait(sh.cmd)
	}
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

func (sh *runningShell) closeFiles() {
	if sh.file != nil {
		proc.CloseLogged(sh.file)
	}
	if sh.snapshot != nil {
		proc.CloseLogged(sh.snapshot)
	}
	if sh.fallback != nil {
		proc.RemoveLogged(sh.fallback.Name())
		proc.CloseLogged(sh.fallback)
	}
}

// reap waits for the process and its output and finalizes the output.
func (sh *runningShell) reap() {
	proc.Wait(sh.cmd)
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
	sh.out.finish()
	if sh.file != nil {
		if err := terminalfile.AppendExit(sh.file, proc.ExitCode(sh.cmd.ProcessState), sh.statusAt); err != nil {
			log.Printf("terminal %d: %v", sh.shellID, err)
		}
		proc.CloseLogged(sh.file)
	}
	close(sh.done)
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
	soft, stopSoft := proc.Timer(sh.args.Timeout)
	defer stopSoft()
	hard, stopHard := proc.Timer(sh.args.GetHardTimeout())
	defer stopHard()
	reason := agentv1.ShellAbortReason_SHELL_ABORT_REASON_UNSPECIFIED
	select {
	case <-sh.done:
	case <-ctx.Done():
		reason = agentv1.ShellAbortReason_SHELL_ABORT_REASON_USER_ABORT
	case <-hard:
		reason = agentv1.ShellAbortReason_SHELL_ABORT_REASON_TIMEOUT
	case <-soft:
		if sh.canBackground() && !sh.exited() {
			stopHard()
			return sh.detach()
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

// detach leaves the command running in the background, still enforcing the
// hard timeout, and tells the agent where its output goes.
func (sh *runningShell) detach() error {
	sh.out.mu.Lock()
	sh.out.detached = true
	sh.out.mu.Unlock()
	if hard := sh.args.GetHardTimeout(); hard > 0 {
		remaining := time.Duration(hard-sh.args.Timeout) * time.Millisecond
		go func() {
			select {
			case <-sh.done:
			case <-time.After(max(remaining, time.Millisecond)):
				proc.Kill(sh.cmd)
			}
		}()
	}
	return sh.out.send(shellMessage(&agentv1.ShellStream{Event: &agentv1.ShellStream_Backgrounded{
		Backgrounded: &agentv1.ShellStreamBackgrounded{
			ShellId:          sh.shellID,
			Command:          sh.args.Command,
			WorkingDirectory: sh.cwd,
			Pid:              proc.PID(sh.cmd),
			Reason:           agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT.Enum(),
		},
	}}))
}

// finish kills the command if it was aborted and reports its exit together
// with the directory the next command starts in. An aborted command's state is
// discarded.
func (sh *runningShell) finish(reason agentv1.ShellAbortReason) error {
	aborted := reason != agentv1.ShellAbortReason_SHELL_ABORT_REASON_UNSPECIFIED
	if aborted {
		proc.Kill(sh.cmd)
		<-sh.done
	}
	if err := sh.out.sendErr(); err != nil {
		return err
	}
	cwd := sh.cwd
	if !aborted {
		st, notice := sh.ex.session.Adopt(sh.snapshot, sh.fallback, sh.cwd)
		if st != nil {
			sh.final, cwd = st, st.Cwd()
		}
		if notice != "" {
			if err := sh.out.send(shellMessage(stderrEvent(notice))); err != nil {
				return err
			}
		}
	}
	code := max(0, proc.ExitCode(sh.cmd.ProcessState))
	return sh.out.send(shellMessage(&agentv1.ShellStream{Event: &agentv1.ShellStream_Exit{Exit: &agentv1.ShellStreamExit{
		Code:        uint32(code), //nolint:gosec // non-negative
		Cwd:         cwd,
		Aborted:     aborted,
		AbortReason: &reason,
	}}}))
}

func (sh *runningShell) canBackground() bool {
	return sh.args.TimeoutBehavior == agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND
}
