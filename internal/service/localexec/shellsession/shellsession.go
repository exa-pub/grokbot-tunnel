// Package shellsession keeps the shell state that commands share, like the
// desktop's shell core executor with its BashState and ZshState (anchor:
// "__CURSOR_BASH_STATE_START__"), and builds the shells that run commands.
//
// Commands share one shell state: cwd, exported environment, options and
// functions. The first command starts in the root, whatever the profile does.
// Commands run in parallel, as on the desktop: each starts from the state of
// the moment it starts, and each that ends on its own writes the state it left
// back, so the command that ends last wins. A "sleep 5; true" started before a
// parallel "cd a" and ending after it puts the old cwd back.
//
// Aliases that a command defines are kept and expanded in later commands, as
// on the desktop. Those of the user's profile are not, unlike there: they are
// meant for people (rm -i) and would make commands behave differently than
// written. The user's profile is read once, by an interactive login shell
// that only dumps its state; every command then runs in a fresh non-login
// shell that replays the state, runs the command and dumps the state again.
// Like the desktop's, that shell starts with the CLI's values of the
// variables it reads at startup, such as $BASH_ENV and $ZDOTDIR, and the
// replay exports the saved ones for the command (startupEnv).
//
// Descriptors of a foreground shell:
//
//	3  replay script (aliases, functions, options), read and closed first
//	4  state snapshot, closed while the command runs
//	5  command text ended by a NUL, read and closed first
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
// snapshot, and the previous state stays in effect silently, as on the
// desktop. The fallback files live in the session's private directory, which
// Close removes, so a shell that outlives the session writes no state.
//
// The state and the command reach the shell through files, not arguments:
// Linux limits a single argument to 128 KiB, and functions from a profile or
// a command with a heredoc can exceed that. Like the desktop's, a shell whose
// startup file ($BASH_ENV, .zshenv) closed or replaced descriptor 5 does not
// run the command, and the session passes commands as arguments from then
// on. One that closed descriptor 3 runs the command without the replay.
package shellsession

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

const (
	// maxSnapshot only guards memory; the desktop has no limit.
	maxSnapshot = 64 << 20
	initTimeout = 10 * time.Second
	// sectionPrefix tags the non-environment parts of a state snapshot.
	sectionPrefix    = "GROK_SHELL_"
	sectionEnvSaved  = sectionPrefix + "ENV_SAVED"
	sectionOptions   = sectionPrefix + "OPTIONS"
	sectionFunctions = sectionPrefix + "FUNCTIONS"
	sectionAliases   = sectionPrefix + "ALIASES"
	sectionEnd       = sectionPrefix + "END"
)

// Session is the shell state shared by the commands of one root.
type Session struct {
	root *pathroot.Root
	// dir is the private directory of the shells' temporary files.
	dir   *tempDir
	mu    sync.Mutex // guards state
	state State
	// initOnce starts reading the user's profile into state; initDone is
	// closed once it is done.
	initOnce sync.Once
	initDone chan struct{}
	// commandArg is set once a startup file broke descriptor 5, like the
	// desktop's commandPipeUnreadable: shells then get the command as an
	// argument.
	commandArg atomic.Bool
}

// New returns a Session whose commands run below root by default, with its
// private directory for temporary files created and locked; Close removes
// it. Private directories that no session holds locked and fallback files
// that no process has open are removed first.
func New(root *pathroot.Root) (*Session, error) {
	dir, err := newTempDir()
	if err != nil {
		return nil, err
	}
	return &Session{root: root, dir: dir}, nil
}

// Close removes the session's private directory and releases its lock.
// Shells still running then save no fallback state.
func (s *Session) Close() error { return s.dir.close() }

// Root is the root the commands run below by default.
func (s *Session) Root() *pathroot.Root { return s.root }

// Cwd resolves the working directory of a command like the desktop's ilt: a
// path that is empty or only whitespace is the state's cwd, taken as it is,
// even if it no longer exists (see Gone); any other path is resolved by the
// root, which runs a missing directory in the root with a warning.
func (s *Session) Cwd(path string, st State) (dir, warning string) {
	if strings.TrimSpace(path) == "" {
		return s.cwd(st), ""
	}
	return s.root.Cwd(path)
}

// CurrentCwd is the cwd of the current state, which a command's exit reports,
// as on the desktop.
func (s *Session) CurrentCwd() string { return s.cwd(s.State()) }

// Gone returns the desktop's message for a command that does not run because
// dir, the state's cwd, is no longer a directory, or "" if it is one. A
// failure to tell is an error.
func Gone(dir string) (string, error) {
	info, err := os.Stat(dir)
	switch {
	case err == nil && info.IsDir():
		return "", nil
	case err == nil, errors.Is(err, fs.ErrNotExist), errors.Is(err, syscall.ENOTDIR):
		return fmt.Sprintf("The shell's working directory no longer exists: %s. The command did not run. "+
			"Re-run it with working_directory set to an existing directory.\n", dir), nil
	}
	return "", err
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
// command, without the profile's aliases. A profile that fails or hangs
// leaves the CLI's environment. Either way the first command starts in the
// root, like the desktop's session, which is cloned into the root after
// reading the profile. If ctx ends first, the
// profile keeps loading for the next command.
func (s *Session) Init(ctx context.Context) error {
	s.initOnce.Do(func() {
		s.initDone = make(chan struct{})
		go func() {
			defer close(s.initDone)
			st, err := s.profileState()
			if err != nil {
				log.Printf("shell profile: %v; using the CLI environment", err)
				st = State{}
			}
			st.cwd, st.aliases = s.root.Dir(), ""
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

// Adopt returns the state a command that started with started left in its
// snapshot or, if it ended the shell itself, in the fallback snapshot of its
// EXIT trap; nil if it left none. Its cwd is kept even if it no longer exists,
// like the desktop's; the next command meant to run there does not run (Gone).
func Adopt(snapshot, fallback *os.File, started State) *State {
	st, ok := readState(snapshot)
	if !ok {
		st, ok = readState(fallback)
	}
	if !ok {
		return nil
	}
	if st.env == nil {
		// /usr/bin/env failed, for example on a variable above the kernel's
		// 128 KiB limit. Keep the environment the command started with.
		st.env = started.env
	}
	return &st
}

// Commit writes the state that a command which started with started and
// ended on its own left in its snapshot files to the session, like the
// desktop's shell state after every command that was not aborted. A command
// that left no state changes nothing, silently, as on the desktop.
func (s *Session) Commit(snapshot, fallback *os.File, started State) {
	if st := Adopt(snapshot, fallback, started); st != nil {
		s.SetState(*st)
	}
}

// StateFiles returns the files a command saves its state to: the snapshot,
// unlinked, and the fallback of its EXIT trap, linked in the session's
// private directory until the command exits. Close them with
// CloseStateFiles.
func (s *Session) StateFiles() (snapshot, fallback *os.File, err error) {
	if snapshot, err = s.tempFile("state-", ""); err != nil {
		return nil, nil, err
	}
	if fallback, err = s.dir.file(func(dir string) (*os.File, error) { return os.CreateTemp(dir, "fallback-") }); err != nil {
		proc.CloseLogged(snapshot)
		return nil, nil, err
	}
	return snapshot, fallback, nil
}

// CloseStateFiles closes and removes the files of StateFiles.
func CloseStateFiles(snapshot, fallback *os.File) {
	proc.CloseLogged(snapshot)
	proc.RemoveLogged(fallback.Name())
	proc.CloseLogged(fallback)
}

// tempFile returns an unlinked file holding content in the session's private
// directory, like proc.TempFile.
func (s *Session) tempFile(prefix, content string) (*os.File, error) {
	return s.dir.file(func(dir string) (*os.File, error) { return proc.TempFile(dir, prefix, content) })
}

func (s *Session) cwd(st State) string {
	if st.cwd == "" {
		return s.root.Dir()
	}
	return st.cwd
}

func (s *Session) profileState() (State, error) {
	snapshot, err := s.tempFile("state-", "")
	if err != nil {
		return State{}, err
	}
	defer proc.CloseLogged(snapshot)
	sh := userShell()
	c := newCmd(sh, State{}, s.root.Dir(), nil, sh.args("-ilc", sh.initScript(), "grokbot-init")...)
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
