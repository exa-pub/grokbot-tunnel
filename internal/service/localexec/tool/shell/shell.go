// Package shell runs foreground commands for the agent and streams their
// output, like the desktop's local-exec shell stream executor
// (LocalShellStreamExecutor): one command at a time in the shared shell
// session, soft and hard timeouts, cancellation, and on a soft timeout with
// TIMEOUT_BEHAVIOR_BACKGROUND a move to the background with the output going
// to a terminal file.
package shell

import (
	"context"
	"time"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/shellsession"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/background"
)

const (
	streamChunk = 16 << 10
	// defaultTimeout is the desktop's soft timeout when none is given (W_r).
	defaultTimeout = 30_000 // ms
	// waitDelay bounds how long a finished command's output is still
	// forwarded while background jobs it started hold its pipes open.
	waitDelay = time.Second
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
// output. Foreground commands run one at a time and share cwd, environment,
// options and functions. On the soft timeout (softTimeout) the command is
// either killed or, with TIMEOUT_BEHAVIOR_BACKGROUND, left running with its
// output going to a terminal file. isBackground only changes the soft
// timeout, as on the desktop.
func (ex *Executor) Execute(ctx context.Context, x *agentv1.ShellArgs, send execstream.Send) error {
	if err := ex.session.Acquire(ctx); err != nil {
		return err
	}
	defer ex.session.Release()
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
	start := &agentv1.ShellStream_Start{Start: &agentv1.ShellStreamStart{Command: &x.Command}}
	if err := send(shellMessage(&agentv1.ShellStream{Event: start})); err != nil {
		return err
	}
	sh, err := ex.startShell(x, cwd, st, send)
	if err != nil {
		return err
	}
	err = sh.wait(ctx)
	if sh.final != nil {
		ex.session.SetState(*sh.final)
	}
	go func() {
		<-sh.done
		shellsession.CloseStateFiles(sh.snapshot, sh.fallback)
	}()
	return err
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

func (ex *Executor) startShell(x *agentv1.ShellArgs, cwd string, st shellsession.State, send execstream.Send) (_ *runningShell, err error) {
	sh := &runningShell{ex: ex, args: x, cwd: cwd, out: &shellOutput{send: send}, done: make(chan struct{}), started: time.Now()}
	sh.out.hold = sh.canBackground()
	if sh.snapshot, sh.fallback, err = shellsession.StateFiles(); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			shellsession.CloseStateFiles(sh.snapshot, sh.fallback)
		}
	}()
	var release func()
	if sh.cmd, release, err = shellsession.Foreground(st, cwd, x.Command, sh.snapshot, sh.fallback); err != nil {
		return nil, err
	}
	defer release() // the child keeps its own descriptors
	sh.stdout = &streamWriter{out: sh.out, event: stdoutEvent}
	sh.stderr = &streamWriter{out: sh.out, event: stderrEvent}
	if err = sh.start(); err != nil {
		return nil, err
	}
	go sh.reap()
	return sh, nil
}
