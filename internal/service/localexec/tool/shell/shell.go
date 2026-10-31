// Package shell runs foreground commands for the agent and streams their
// output, like the desktop's local-exec shell stream executor: one command at
// a time in the shared shell session, soft and hard timeouts, cancellation,
// and on a soft timeout with TIMEOUT_BEHAVIOR_BACKGROUND a move to the
// background with the output going to a terminal file.
package shell

import (
	"context"
	"os"
	"time"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/shellsession"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/background"
)

const (
	streamChunk = 16 << 10
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
// options and functions. On soft timeout the command is either killed or, with
// TIMEOUT_BEHAVIOR_BACKGROUND, left running with output going to a terminal
// file.
func (ex *Executor) Execute(ctx context.Context, x *agentv1.ShellArgs, send execstream.Send) error {
	if x.IsBackground {
		v, err := ex.background.Spawn(ctx, x.Command, x.WorkingDirectory)
		if err != nil {
			return err
		}
		return send(shellMessage(&agentv1.ShellStream{Event: &agentv1.ShellStream_Backgrounded{
			Backgrounded: &agentv1.ShellStreamBackgrounded{
				ShellId: v.ShellId, Command: v.Command, WorkingDirectory: v.WorkingDirectory, Pid: v.Pid,
			},
		}}))
	}
	if err := ex.session.Acquire(ctx); err != nil {
		return err
	}
	defer ex.session.Release()
	if err := ex.session.Init(ctx); err != nil {
		return err
	}
	st := ex.session.State()
	cwd, warning, err := ex.session.Cwd(x.WorkingDirectory, st)
	if err != nil {
		return err
	}
	if warning != "" {
		if err = send(shellMessage(stderrEvent(warning))); err != nil {
			return err
		}
	}
	start := &agentv1.ShellStream_Start{Start: &agentv1.ShellStreamStart{Command: &x.Command}}
	if err = send(shellMessage(&agentv1.ShellStream{Event: start})); err != nil {
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
	// A backgrounded command's state is discarded: the next command starts
	// from the current state.
	go func() {
		<-sh.done
		proc.CloseLogged(sh.snapshot)
		proc.CloseLogged(sh.fallback)
	}()
	return err
}

func (ex *Executor) startShell(x *agentv1.ShellArgs, cwd string, st shellsession.State, send execstream.Send) (_ *runningShell, err error) {
	sh := &runningShell{ex: ex, args: x, cwd: cwd, out: &shellOutput{send: send}, done: make(chan struct{})}
	defer func() {
		if err != nil {
			sh.closeFiles()
		}
	}()
	if sh.canBackground() {
		if sh.shellID, sh.file, err = ex.background.Terminal(); err != nil {
			return nil, err
		}
		sh.out.file = sh.file
	}
	if sh.snapshot, err = proc.TempFile("grok-shell-state-", ""); err != nil {
		return nil, err
	}
	if sh.fallback, err = os.CreateTemp("", "grok-shell-state-"); err != nil {
		return nil, err
	}
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
