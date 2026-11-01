// Package shellsession keeps the shell state that foreground commands share,
// like the desktop's shell core executor with its BashState and ZshState
// (anchor: "__CURSOR_BASH_STATE_START__"), and builds the shells that run
// commands.
//
// Foreground commands share one shell state: cwd, exported environment,
// options and functions. Aliases are not kept or expanded: they come from
// interactive profiles, are meant for people (rm -i) and would make commands
// behave differently than written. The user's profile is read once, by an
// interactive login shell that only dumps its state; every command then runs
// in a fresh non-login shell that replays the state, runs the command and
// dumps the state again.
//
// Descriptors of a foreground shell:
//
//	3  replay script (options, functions), read and closed first
//	4  state snapshot, closed while the command runs
//	5  command text, read and closed first
//
// The dump runs after the command, not from an EXIT trap: a trap can be
// replaced by the command, and when the command exits from a function or is
// killed by a signal, bash runs it with the command's descriptors, so a
// command that ran "exec 4>&1" would print the whole environment. The command
// runs as "eval ... 4>&-"; bash and zsh restore descriptor 4 after eval even if
// the command reassigned it, and children never inherit it.
//
// A command that ends the shell itself (exit, set -e) is still saved by an
// EXIT trap, as a fallback. The trap writes to a file named by a read-only
// variable, never to an inherited descriptor, so the command cannot redirect
// it; it does nothing in subshells or once the regular dump has run. A command
// that replaces the trap, execs another program or is killed leaves no
// snapshot, and the previous state stays in effect; the agent is told so.
//
// The state and the command reach the shell through files, not arguments:
// Linux limits a single argument to 128 KiB, and functions from a profile or
// a command with a heredoc can exceed that.
package shellsession

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

const (
	maxSnapshot = 1 << 20
	initTimeout = 10 * time.Second
	// sectionPrefix tags the non-environment parts of a state snapshot.
	sectionPrefix    = "GROK_SHELL_"
	sectionEnvSaved  = sectionPrefix + "ENV_SAVED"
	sectionOptions   = sectionPrefix + "OPTIONS"
	sectionFunctions = sectionPrefix + "FUNCTIONS"
	sectionEnd       = sectionPrefix + "END"
)

// Session is the shell state shared by the foreground commands of one root.
// Foreground commands run one at a time, so that each sees the state the
// previous one left.
type Session struct {
	root *pathroot.Root
	// foreground holds a token while a foreground command runs. Waiting for
	// it can be canceled.
	foreground chan struct{}
	mu         sync.Mutex // guards state
	state      State
	// initOnce starts reading the user's profile into state; initDone is
	// closed once it is done.
	initOnce sync.Once
	initDone chan struct{}
}

// New returns a Session whose commands run below root by default.
func New(root *pathroot.Root) *Session {
	return &Session{root: root, foreground: make(chan struct{}, 1)}
}

// Acquire waits until no other foreground command runs. Release ends the
// turn.
func (s *Session) Acquire(ctx context.Context) error {
	select {
	case s.foreground <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	// select picks at random when both are ready.
	if err := ctx.Err(); err != nil {
		s.Release()
		return err
	}
	return nil
}

// Release ends the turn of the foreground command that called Acquire.
func (s *Session) Release() { <-s.foreground }

// Cwd resolves the working directory of a command: path, or the state's cwd
// if path is empty.
func (s *Session) Cwd(path string, st State) (dir, warning string, err error) {
	if path == "" && st.cwd != "" {
		path = st.cwd
	}
	return s.root.Cwd(path)
}

// State returns the current state.
func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// SetState replaces the current state.
func (s *Session) SetState(st State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = st
}

// Init captures the state of the user's profile once, before the first
// command. A profile that fails or hangs leaves the CLI's environment. If ctx
// ends first, the profile keeps loading for the next command.
func (s *Session) Init(ctx context.Context) error {
	s.initOnce.Do(func() {
		s.initDone = make(chan struct{})
		go func() {
			defer close(s.initDone)
			st, err := s.profileState()
			if err != nil {
				log.Printf("shell profile: %v; using the CLI environment", err)
				return
			}
			if !s.root.IsDir(st.cwd) {
				st.cwd = ""
			}
			s.SetState(st)
		}()
	})
	select {
	case <-s.initDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Adopt returns the state a foreground command left in its snapshot or,
// if it ended the shell itself, in the fallback snapshot of its EXIT trap,
// together with a notice for the agent, if any. A command that left no state
// returns nil. A cwd outside Root is not kept; the next command starts where
// this one did.
func (s *Session) Adopt(snapshot, fallback *os.File, cwd string) (*State, string) {
	st, ok := readState(snapshot)
	if !ok {
		st, ok = readState(fallback)
	}
	if !ok {
		return nil, "The shell exited before its state could be saved (exec, a signal or a replaced EXIT trap); " +
			"changes to the working directory, environment, options and functions were not kept.\n"
	}
	notice := ""
	if st.env == nil {
		// /usr/bin/env failed, for example on a variable above the kernel's
		// 128 KiB limit. Keep the environment the command started with.
		st.env = s.State().env
		notice = "The environment could not be saved after this command; the next command starts with the " +
			"environment this one started with.\n"
	}
	if !s.root.IsDir(st.cwd) {
		notice += fmt.Sprintf("Working directory %s is outside %s or no longer exists; the next command starts in %s.\n",
			st.cwd, s.root.Dir(), cwd)
		st.cwd = cwd
	}
	return &st, notice
}

func (s *Session) profileState() (State, error) {
	snapshot, err := proc.TempFile("grok-shell-state-", "")
	if err != nil {
		return State{}, err
	}
	defer proc.CloseLogged(snapshot)
	sh := userShell()
	c := newCmd(sh, State{}, s.root.Dir(), sh.args("-ilc", sh.initScript(), "grokbot-init")...)
	// A new session keeps the interactive shell away from the CLI's terminal.
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	c.ExtraFiles = []*os.File{nil, snapshot}
	if err = c.Start(); err != nil {
		return State{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			proc.Kill(c)
		case <-done:
		}
	}()
	proc.Wait(c)
	close(done)
	if ctx.Err() != nil {
		return State{}, errors.New("profile did not finish within 10s")
	}
	b, err := readSnapshot(snapshot)
	if err != nil {
		return State{}, err
	}
	st, ok := parseSnapshot(b)
	if !ok {
		return State{}, errors.New("profile left no state")
	}
	return st, nil
}
