// Package background runs commands in the background with their output in a
// terminal file, like the desktop's local-exec background shell executor and
// its BackgroundShellManager: a shell ID per command, a header before any
// output and a footer once the command exits.
package background

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/denylist"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/sandbox"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/shellsession"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
)

const (
	// The range of shell IDs, like the desktop's (yN, Dhe).
	firstID = 1000
	lastID  = 999_999
	// waitDelay bounds how long a finished command's output is still
	// collected while background jobs it started hold its pipes open, like
	// the desktop's closeTimeout.
	waitDelay = 5 * time.Second
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

// Execute starts args.Command and sends its shell ID, or, like the desktop,
// the refusal of a command that an administrator's denylist blocks or, on
// Linux, of one that is to run in a sandbox.
func (bg *Executor) Execute(ctx context.Context, args *agentv1.BackgroundShellSpawnArgs, send execstream.Send) error {
	result := &agentv1.BackgroundShellSpawnResult{}
	rejected, err := bg.denied(ctx, args)
	if err != nil {
		return err
	}
	var refusal *agentv1.ShellSandboxUnsupported
	if rejected == nil {
		refusal, err = bg.sandboxed(ctx, args)
	}
	switch {
	case err != nil:
		return err
	case rejected != nil:
		result.Result = &agentv1.BackgroundShellSpawnResult_Rejected{Rejected: rejected}
	case refusal != nil:
		result.Result = &agentv1.BackgroundShellSpawnResult_SandboxUnsupported{SandboxUnsupported: refusal}
	default:
		success, err := bg.Spawn(ctx, args)
		if err != nil {
			return err
		}
		result.Result = &agentv1.BackgroundShellSpawnResult_Success{Success: success}
	}
	return send(&agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_BackgroundShellSpawnResult{
		BackgroundShellSpawnResult: result,
	}})
}

// Spawn starts args.Command in its own session with output going to a
// terminal file. Its output goes through pipes, so that each stream is cut at
// terminalfile.MaxStream like the desktop's.
// It starts from the session's state without waiting for running foreground
// commands, and like the desktop, writes the state it leaves back to the
// session when it exits on its own: the command that ends last wins. A
// working directory that does not exist is replaced by the default one
// silently, as on the desktop; if no directory is given and the session's no
// longer exists, the command does not run and its terminal file says so, as
// on the desktop. Descriptors are those of a foreground shell, plus 6, a gate
// opened once the terminal header is written.
func (bg *Executor) Spawn(ctx context.Context, args *agentv1.BackgroundShellSpawnArgs) (_ *agentv1.BackgroundShellSpawnSuccess, err error) {
	if err = bg.session.Init(ctx); err != nil {
		return nil, err
	}
	cmd := args.Command
	st := bg.session.State()
	cwd, _ := bg.session.Cwd(args.WorkingDirectory, st)
	header := terminalfile.Command{Cwd: cwd, Command: cmd, Title: args.GetDescription(), Started: time.Now()}
	gone, err := goneCwd(args.WorkingDirectory, cwd)
	if err != nil {
		return nil, err
	}
	n, f, err := bg.Terminal()
	if err != nil {
		return nil, err
	}
	if gone != "" {
		return bg.refuse(n, f, header, gone)
	}
	snapshot, fallback, err := bg.session.StateFiles()
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
	env := shellsession.RequestEnv(bg.root.Dir(), args.GetConversationId(), args.GetRequestId())
	c, release, err := bg.session.Background(st, cwd, cmd, env, gate, snapshot, fallback)
	if err != nil {
		return nil, err
	}
	defer release() // the child keeps its own descriptors
	tf, out, err := launch(c, f, open, header)
	if err != nil {
		return nil, err
	}
	go bg.finish(c, n, tf, out, st, snapshot, fallback)
	return &agentv1.BackgroundShellSpawnSuccess{ShellId: n, Command: cmd, WorkingDirectory: cwd, Pid: proc.PID(c)}, nil
}

// goneCwd returns the desktop's message if no working directory is given
// and cwd, the session's, no longer exists.
func goneCwd(path, cwd string) (string, error) {
	if strings.TrimSpace(path) != "" {
		return "", nil
	}
	return shellsession.Gone(cwd)
}

// launch starts c with its output going to terminal file f for cmd, then
// opens the gate the command waits on, so that the header comes first.
func launch(c *exec.Cmd, f, gate *os.File, cmd terminalfile.Command) (*terminalfile.File, *output, error) {
	out, err := newOutput(c)
	if err != nil {
		return nil, nil, err
	}
	err = c.Start()
	out.started()
	if err != nil {
		out.close()
		return nil, nil, err
	}
	cmd.PID = c.Process.Pid
	tf, err := terminalfile.Start(f, cmd, "")
	if err == nil {
		_, err = gate.WriteString("\n")
	}
	if err != nil {
		if tf != nil {
			tf.Stop()
		}
		proc.Kill(c)
		proc.Wait(c)
		out.close()
		return nil, nil, err
	}
	out.drain(tf)
	return tf, out, nil
}

// Terminal takes the next shell ID that no running command has and creates
// or truncates its terminal file <root>/terminals/<n>.txt, like the desktop.
// The ID is taken until Release.
func (bg *Executor) Terminal() (uint32, *os.File, error) {
	if err := bg.root.MkdirAll(terminalfile.Dir, 0o700); err != nil {
		return 0, nil, err
	}
	n, err := bg.take()
	if err != nil {
		return 0, nil, err
	}
	f, err := bg.root.OpenFile(fmt.Sprintf("%s/%d.txt", terminalfile.Dir, n), os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
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
	return bg.root.Remove(fmt.Sprintf("%s/%d.txt", terminalfile.Dir, n))
}

// denied returns the refusal of a command that the denylist of its request
// blocks (see denylist), like the desktop's: before anything is started or
// written.
func (bg *Executor) denied(ctx context.Context, args *agentv1.BackgroundShellSpawnArgs) (*agentv1.ShellRejected, error) {
	reason := denylist.Refusal(args.Command, args.AdminCommandDenylist)
	if reason == "" {
		return nil, nil //nolint:nilnil // no refusal
	}
	if err := bg.session.Init(ctx); err != nil {
		return nil, err
	}
	cwd, _ := bg.session.Cwd(args.WorkingDirectory, bg.session.State())
	return &agentv1.ShellRejected{Command: args.Command, WorkingDirectory: cwd, Reason: reason}, nil
}

// sandboxed returns the refusal of a command whose request asks for a
// sandbox, which neither the desktop on Linux nor grokbot-tunnel has (see
// sandbox), and writes it to a terminal file of the command's own, as the
// desktop does. A command whose session cwd no longer exists is left to
// Spawn, which says so first, like the desktop.
func (bg *Executor) sandboxed(ctx context.Context, args *agentv1.BackgroundShellSpawnArgs) (*agentv1.ShellSandboxUnsupported, error) {
	policy := sandbox.Effective(args.SandboxPolicy, args.SkipApproval)
	if sandbox.Unsupported(policy, "", "") == nil {
		return nil, nil //nolint:nilnil // no refusal
	}
	if err := bg.session.Init(ctx); err != nil {
		return nil, err
	}
	cwd, _ := bg.session.Cwd(args.WorkingDirectory, bg.session.State())
	if gone, err := goneCwd(args.WorkingDirectory, cwd); err != nil || gone != "" {
		return nil, err
	}
	refusal := sandbox.Unsupported(policy, args.Command, cwd)
	n, f, err := bg.Terminal()
	if err != nil {
		return nil, err
	}
	defer bg.Release(n)
	header := terminalfile.Command{Cwd: cwd, Command: args.Command, Title: args.GetDescription(), Started: time.Now()}
	tf, err := terminalfile.Start(f, header, "")
	if err != nil {
		proc.CloseLogged(f)
		return nil, err
	}
	return refusal, tf.Fail(sandbox.Error(refusal))
}

// refuse writes the terminal file f of shell ID n for a command that does not
// run, with message as its output and exit code 1, like the desktop's for a
// session cwd that no longer exists: without a pid, also in the result.
func (bg *Executor) refuse(n uint32, f *os.File, cmd terminalfile.Command, message string) (*agentv1.BackgroundShellSpawnSuccess, error) {
	defer bg.Release(n)
	tf, err := terminalfile.Start(f, cmd, message)
	if err != nil {
		proc.CloseLogged(f)
		return nil, err
	}
	if err := tf.Exit("1", false); err != nil {
		return nil, err
	}
	return &agentv1.BackgroundShellSpawnSuccess{ShellId: n, Command: cmd.Command, WorkingDirectory: cmd.Cwd}, nil
}

// finish waits for background command c and writes the state it left to the
// session, then the footer of its terminal file. The state comes first, as on
// the desktop: whoever sees the footer sees the state.
func (bg *Executor) finish(
	c *exec.Cmd, n uint32, tf *terminalfile.File, out *output, started shellsession.State, snapshot, fallback *os.File,
) {
	proc.Wait(c)
	proc.Forward(c) // nothing aborts a background command here
	bg.session.Exited(c, snapshot, fallback)
	out.wait(waitDelay)
	bg.session.Commit(snapshot, fallback, started)
	shellsession.CloseStateFiles(snapshot, fallback)
	if err := tf.Exit(terminalfile.Code(c.ProcessState), false); err != nil {
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

// output holds the pipes of a command's stdout and stderr.
type output struct {
	stdout, stdoutW *os.File
	stderr, stderrW *os.File
	drained         sync.WaitGroup
}

// newOutput connects c's stdout and stderr to new pipes.
func newOutput(c *exec.Cmd) (*output, error) {
	o := &output{}
	var err error
	if o.stdout, o.stdoutW, err = os.Pipe(); err != nil {
		return nil, err
	}
	if o.stderr, o.stderrW, err = os.Pipe(); err != nil {
		proc.CloseLogged(o.stdout)
		proc.CloseLogged(o.stdoutW)
		return nil, err
	}
	c.Stdout, c.Stderr = o.stdoutW, o.stderrW
	return o, nil
}

// started closes the write ends once the command has them, so that the
// readers see EOF once it and its children are gone.
func (o *output) started() {
	proc.CloseLogged(o.stdoutW)
	proc.CloseLogged(o.stderrW)
}

// close closes the read ends of a command whose output is not drained.
func (o *output) close() {
	proc.CloseLogged(o.stdout)
	proc.CloseLogged(o.stderr)
}

// drain copies the output into the streams of tf until EOF. Output after the
// footer is read and dropped, so that background jobs never block on it.
func (o *output) drain(tf *terminalfile.File) {
	o.drained.Add(2)
	for name, r := range map[string]*os.File{"stdout": o.stdout, "stderr": o.stderr} {
		go func() {
			defer o.drained.Done()
			defer proc.CloseLogged(r)
			if _, err := io.Copy(tf.Stream(name), r); err != nil {
				log.Printf("background output: %v", err)
			}
		}()
	}
}

// wait waits until the output is drained, at most delay.
func (o *output) wait(delay time.Duration) {
	done := make(chan struct{})
	go func() {
		o.drained.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(delay):
	}
}
