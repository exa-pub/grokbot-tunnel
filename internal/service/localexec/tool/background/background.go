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
	"math/rand/v2"
	"os"
	"os/exec"
	"sync"
	"time"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/shellsession"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
)

// The range of shell IDs, like the desktop's (yN, Dhe).
const (
	firstID = 1000
	lastID  = 999_999
)

// Executor starts background commands below a root.
type Executor struct {
	root    *pathroot.Root
	session *shellsession.Session

	mu sync.Mutex
	// next is the next shell ID to try; it starts at random, like the
	// desktop's.
	next uint32
	// running holds the IDs of the commands still running.
	running map[uint32]bool
}

// New returns an Executor whose commands replay session's state.
func New(root *pathroot.Root, session *shellsession.Session) *Executor {
	return &Executor{
		root:    root,
		session: session,
		next:    firstID + rand.Uint32N(lastID-firstID+1), //nolint:gosec // an ID, not a secret
		running: map[uint32]bool{},
	}
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

// Spawn starts cmd in its own session with output going to a terminal file.
// It starts from the session's state without waiting for running foreground
// commands, and like the desktop, writes the state it leaves back to the
// session when it exits on its own: the command that ends last wins. A
// working directory that does not exist is replaced by the default one
// silently, as on the desktop. Descriptors are those of a foreground shell,
// plus 6, a gate opened once the terminal header is written.
func (bg *Executor) Spawn(ctx context.Context, cmd, path string) (_ *agentv1.BackgroundShellSpawnSuccess, err error) {
	if err = bg.session.Init(ctx); err != nil {
		return nil, err
	}
	st := bg.session.State()
	cwd, _ := bg.session.Cwd(path, st)
	started := time.Now()
	n, f, err := bg.Terminal()
	if err != nil {
		return nil, err
	}
	snapshot, fallback, err := shellsession.StateFiles()
	if err != nil {
		proc.CloseLogged(f)
		bg.Release(n)
		return nil, err
	}
	defer func() {
		if err != nil {
			proc.CloseLogged(f)
			shellsession.CloseStateFiles(snapshot, fallback)
			bg.Release(n)
		}
	}()
	gate, open, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer gate.Close()
	defer open.Close()
	c, release, err := shellsession.Background(st, cwd, cmd, gate, snapshot, fallback)
	if err != nil {
		return nil, err
	}
	defer release() // the child keeps its own descriptors
	c.Stdout = f
	c.Stderr = f
	if err = c.Start(); err != nil {
		return nil, err
	}
	tf, err := terminalfile.Start(f, terminalfile.Command{PID: c.Process.Pid, Cwd: cwd, Command: cmd, Started: started}, nil)
	if err == nil {
		_, err = open.WriteString("\n")
	}
	if err != nil {
		proc.Kill(c)
		proc.Wait(c)
		return nil, err
	}
	go bg.finish(c, n, tf, cwd, snapshot, fallback)
	return &agentv1.BackgroundShellSpawnSuccess{ShellId: n, Command: cmd, WorkingDirectory: cwd, Pid: proc.PID(c)}, nil
}

// Terminal takes the next shell ID that no running command has and creates
// or truncates its terminal file <root>/terminals/<n>.txt, like the desktop.
// The ID is taken until Release.
func (bg *Executor) Terminal() (uint32, *os.File, error) {
	if err := bg.root.FS().MkdirAll(terminalfile.Dir, 0o700); err != nil {
		return 0, nil, err
	}
	n, err := bg.take()
	if err != nil {
		return 0, nil, err
	}
	f, err := bg.root.FS().OpenFile(fmt.Sprintf("%s/%d.txt", terminalfile.Dir, n), os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		bg.Release(n)
		return 0, nil, err
	}
	return n, f, nil
}

// Release frees shell ID n once its command has ended.
func (bg *Executor) Release(n uint32) {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	delete(bg.running, n)
}

// Remove deletes the terminal file of shell ID n, taken by Terminal but not
// used, and releases the ID.
func (bg *Executor) Remove(n uint32) error {
	defer bg.Release(n)
	return bg.root.FS().Remove(fmt.Sprintf("%s/%d.txt", terminalfile.Dir, n))
}

// finish waits for background command c and writes the state it left to the
// session, then the footer of its terminal file. The state comes first, as on
// the desktop: whoever sees the footer sees the state.
func (bg *Executor) finish(c *exec.Cmd, n uint32, tf *terminalfile.File, cwd string, snapshot, fallback *os.File) {
	proc.Wait(c)
	bg.session.Commit(snapshot, fallback, cwd)
	shellsession.CloseStateFiles(snapshot, fallback)
	if err := tf.Exit(c.ProcessState, false); err != nil {
		log.Printf("terminal %d: %v", n, err)
	}
	bg.Release(n)
}

// take returns the next free shell ID (generateShellId).
func (bg *Executor) take() (uint32, error) {
	bg.mu.Lock()
	defer bg.mu.Unlock()
	for range lastID - firstID + 1 {
		if bg.next > lastID {
			bg.next = firstID
		}
		n := bg.next
		bg.next++
		if !bg.running[n] {
			bg.running[n] = true
			return n, nil
		}
	}
	return 0, errors.New("No available background shell id in range") //nolint:staticcheck // the desktop's text
}
