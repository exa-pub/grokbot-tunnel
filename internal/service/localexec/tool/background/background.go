// Package background runs commands in the background with their output in a
// terminal file, like the desktop's local-exec background shell executor and
// its BackgroundShellManager: a shell ID per command, a header before any
// output and a footer once the command exits.
package background

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync/atomic"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/shellsession"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
)

// Executor starts background commands below a root.
type Executor struct {
	root    *pathroot.Root
	session *shellsession.Session
	// last numbers the terminal files in <root>/terminals.
	last atomic.Uint32
}

// New returns an Executor whose commands replay session's state.
func New(root *pathroot.Root, session *shellsession.Session) *Executor {
	return &Executor{root: root, session: session}
}

// Execute starts args.Command and sends its shell ID.
func (bg *Executor) Execute(ctx context.Context, args *agentv1.BackgroundShellSpawnArgs, send execstream.Send) error {
	result, err := bg.Spawn(ctx, args.Command, args.WorkingDirectory)
	if err != nil {
		return err
	}
	return send(&agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_BackgroundShellSpawnResult{
		BackgroundShellSpawnResult: &agentv1.BackgroundShellSpawnResult{
			Result: &agentv1.BackgroundShellSpawnResult_Success{Success: result},
		},
	}})
}

// Spawn starts cmd in its own session with output going to a terminal
// file. It replays the foreground state but neither waits for running
// foreground commands nor changes the state. Descriptors are those of a
// foreground shell, except that 4 is a gate opened once the terminal header is
// written; the footer is appended when the process exits. A warning about the
// working directory follows the header.
func (bg *Executor) Spawn(ctx context.Context, cmd, path string) (_ *agentv1.BackgroundShellSpawnSuccess, err error) {
	if err = bg.session.Init(ctx); err != nil {
		return nil, err
	}
	st := bg.session.State()
	cwd, warning, err := bg.session.Cwd(path, st)
	if err != nil {
		return nil, err
	}
	n, f, err := bg.Terminal()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			proc.CloseLogged(f)
		}
	}()
	gate, open, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	defer open.Close()
	c, release, err := shellsession.Background(st, cwd, cmd, gate)
	if err != nil {
		return nil, err
	}
	defer release() // the child keeps its own descriptors
	c.Stdout = f
	c.Stderr = f
	if err = c.Start(); err != nil {
		return nil, err
	}
	statusAt, err := terminalfile.WriteHeader(f, cwd, cmd, c.Process.Pid, warning)
	if err == nil {
		_, err = open.WriteString("\n")
	}
	if err != nil {
		proc.Kill(c)
		proc.Wait(c)
		return nil, err
	}
	go closeTerminal(n, f, c, statusAt)
	return &agentv1.BackgroundShellSpawnSuccess{ShellId: n, Command: cmd, WorkingDirectory: cwd, Pid: proc.PID(c)}, nil
}

// closeTerminal appends the footer to terminal file n once c has exited.
func closeTerminal(n uint32, f *os.File, c *exec.Cmd, statusAt int64) {
	proc.Wait(c)
	if err := terminalfile.AppendExit(f, proc.ExitCode(c.ProcessState), statusAt); err != nil {
		log.Printf("terminal %d: %v", n, err)
	}
	proc.CloseLogged(f)
}

// Terminal creates the next free <root>/terminals/<n>.txt output file and
// returns its number, which is also the shell ID.
func (bg *Executor) Terminal() (uint32, *os.File, error) {
	if err := bg.root.FS().MkdirAll(terminalfile.Dir, 0o700); err != nil {
		return 0, nil, err
	}
	for {
		n := bg.last.Add(1)
		f, err := bg.root.FS().OpenFile(fmt.Sprintf("%s/%d.txt", terminalfile.Dir, n), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return n, f, err
	}
}
