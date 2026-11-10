// Package shell runs foreground commands for the agent and streams their
// output, like the desktop's local-exec shell stream executor
// (LocalShellStreamExecutor): commands run in parallel in the shared shell
// session, with soft and hard timeouts, cancellation, and on a soft timeout
// with TIMEOUT_BEHAVIOR_BACKGROUND a move to the background with the output
// going to a terminal file.
package shell

import (
	"context"
	"math"
	"strings"
	"time"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/denylist"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/sandbox"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/shellsession"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/background"
)

const (
	streamChunk = 16 << 10
	// defaultTimeout is the desktop's soft timeout when none is given (W_r).
	defaultTimeout = 30_000 // ms
	// waitDelay bounds how long a finished command's output is still
	// forwarded while background jobs it started hold its pipes open, like
	// the desktop's closeTimeout.
	waitDelay = 5 * time.Second
)

// Executor runs foreground commands in a shell session.
type Executor struct {
	session    *shellsession.Session
	background *background.Executor
}

// New returns an Executor for session; background starts the commands that
// isBackground or a soft timeout move to the background.
func New(session *shellsession.Session, background *background.Executor) *Executor {
	return &Executor{session: session, background: background}
}

// Execute runs a foreground command in its own process group and streams its
// output. Commands run in parallel, each from the session's state of the
// moment it starts (see shellsession). On the soft timeout (softTimeout) the
// command is either killed or, with TIMEOUT_BEHAVIOR_BACKGROUND, left running
// with its output going to a terminal file. isBackground only changes the
// soft timeout, as on the desktop. Without a working directory, a command
// whose session cwd no longer exists does not run, as on the desktop, and
// neither does a command that an administrator's denylist blocks (see
// denylist) or one that is to run in a sandbox (see sandbox).
func (ex *Executor) Execute(ctx context.Context, x *agentv1.ShellArgs, send execstream.Send) error {
	if err := ex.session.Init(ctx); err != nil {
		return err
	}
	st := ex.session.State()
	cwd, warning := ex.session.Cwd(x.WorkingDirectory, st)
	if warning != "" {
		if err := send(shellMessage(stderrEvent(warning))); err != nil {
			return err
		}
	}
	// Like the desktop, which checks an administrator's denylist before
	// anything else.
	if reason := denylist.Refusal(x.Command, x.AdminCommandDenylist); reason != "" {
		rejected := &agentv1.ShellRejected{Command: x.Command, WorkingDirectory: cwd, Reason: reason}
		return send(shellMessage(&agentv1.ShellStream{Event: &agentv1.ShellStream_Rejected{Rejected: rejected}}))
	}
	// Like the desktop's after local approval: no command, and the sandbox
	// that was asked for, which is none unless the request names one.
	policy := sandbox.Effective(x.RequestedSandboxPolicy, x.SkipApproval)
	start := &agentv1.ShellStream_Start{Start: &agentv1.ShellStreamStart{SandboxPolicy: policy}}
	if err := send(shellMessage(&agentv1.ShellStream{Event: start})); err != nil {
		return err
	}
	started := time.Now()
	if strings.TrimSpace(x.WorkingDirectory) == "" {
		gone, err := shellsession.Gone(cwd)
		if err != nil {
			return err
		}
		if gone != "" {
			if err := send(shellMessage(stderrEvent(gone))); err != nil {
				return err
			}
			exit := &agentv1.ShellStreamExit{Code: 1, Cwd: ex.session.CurrentCwd(), LocalExecutionTimeMs: elapsedMs(started)}
			return send(shellMessage(&agentv1.ShellStream{Event: &agentv1.ShellStream_Exit{Exit: exit}}))
		}
	}
	// Like the desktop on Linux, which has no sandbox to run it in.
	if refusal := sandbox.Unsupported(policy, x.Command, cwd); refusal != nil {
		return send(shellMessage(&agentv1.ShellStream{Event: &agentv1.ShellStream_SandboxUnsupported{SandboxUnsupported: refusal}}))
	}
	sh, err := ex.startShell(x, cwd, st, started, send)
	if err != nil {
		return err
	}
	err = sh.wait(ctx)
	go func() {
		<-sh.done
		shellsession.CloseStateFiles(sh.snapshot, sh.fallback)
	}()
	return err
}

// elapsedMs is localExecutionTimeMs of the desktop's exit: the milliseconds
// since start, rounded.
func elapsedMs(start time.Time) *int32 {
	ms := time.Since(start).Round(time.Millisecond).Milliseconds()
	v := int32(min(max(ms, 0), math.MaxInt32)) //nolint:gosec // clamped
	return &v
}

// softTimeout returns the soft timeout in milliseconds, like the desktop's
// Nye: the given one, else 0 for background commands, BACKGROUND and commands
// with a hard timeout, else 30 s. Without BACKGROUND a timeout of 0 aborts the
// command at once, as on the desktop.
func softTimeout(x *agentv1.ShellArgs) int32 {
	switch {
	case x.Timeout != 0:
		return x.Timeout
	case x.IsBackground || x.TimeoutBehavior == agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND || x.GetHardTimeout() > 0:
		return 0
	}
	return defaultTimeout
}

func (ex *Executor) startShell(
	x *agentv1.ShellArgs, cwd string, st shellsession.State, started time.Time, send execstream.Send,
) (_ *runningShell, err error) {
	sh := &runningShell{ex: ex, args: x, cwd: cwd, state: st, out: &shellOutput{send: send}, done: make(chan struct{}), started: started}
	sh.out.hold = sh.canBackground()
	if threshold := x.GetFileOutputThresholdBytes(); threshold > 0 {
		sh.out.full = &fullOutput{root: ex.session.Root(), threshold: threshold}
	}
	if sh.snapshot, sh.fallback, err = ex.session.StateFiles(); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			shellsession.CloseStateFiles(sh.snapshot, sh.fallback)
		}
	}()
	env := shellsession.RequestEnv(ex.session.Root().Dir(), x.GetConversationId(), x.GetRequestId())
	var release func()
	if sh.cmd, release, err = ex.session.Foreground(st, cwd, x.Command, env, sh.snapshot, sh.fallback); err != nil {
		return nil, err
	}
	defer release() // the child keeps its own descriptors
	sh.stdout = &streamWriter{out: sh.out, name: "stdout", event: stdoutEvent}
	sh.stderr = &streamWriter{out: sh.out, name: "stderr", event: stderrEvent}
	if err = sh.start(); err != nil {
		return nil, err
	}
	go sh.reap()
	return sh, nil
}
