package shellsession

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
)

// commandPipeBroken is the exit code of a shell that could not read its
// command from descriptor 5, like the desktop's Iqe.
const commandPipeBroken = 126

// Foreground returns the shell that runs cmd in cwd with st replayed and
// env (RequestEnv) added, and saves the state it leaves to snapshot, or from
// its EXIT trap to fallback. release closes the input files once the shell
// has started or failed to. Pass the shell to Exited once it has exited.
func (s *Session) Foreground(
	st State, cwd, cmd string, env []string, snapshot, fallback *os.File,
) (c *exec.Cmd, release func(), err error) {
	piped, sh := s.pipeCommand(), userShell()
	replay, command, err := s.inputFiles(sh, st, cmd, piped)
	if err != nil {
		return nil, nil, err
	}
	c = newCmd(sh, st, cwd, env, sh.args("-c", sh.foregroundScript(piped), "grokbot-shell", commandArgs(fallback, cmd, piped)...)...)
	c.ExtraFiles = []*os.File{replay, snapshot, command}
	return detached(c), releaseInputs(replay, command), nil
}

// Background returns the shell that runs cmd in cwd with st replayed and env
// added once a line is written to the other end of gate, and saves its state
// like Foreground. release closes the input files once the shell has started
// or failed to. Pass the shell to Exited once it has exited.
func (s *Session) Background(
	st State, cwd, cmd string, env []string, gate, snapshot, fallback *os.File,
) (c *exec.Cmd, release func(), err error) {
	piped, sh := s.pipeCommand(), userShell()
	replay, command, err := s.inputFiles(sh, st, cmd, piped)
	if err != nil {
		return nil, nil, err
	}
	c = newCmd(sh, st, cwd, env, sh.args("-c", sh.backgroundScript(piped), "grok-background", commandArgs(fallback, cmd, piped)...)...)
	c.ExtraFiles = []*os.File{replay, snapshot, command, gate}
	return detached(c), releaseInputs(replay, command), nil
}

// Exited notes how shell c of Foreground or Background, which has exited,
// ended. Like the desktop's U3, a shell that was to read its command from
// descriptor 5 and exited with 126 without saving any state was stopped by a
// startup file that closed or replaced the descriptor: the session passes
// commands as arguments from then on.
func (s *Session) Exited(c *exec.Cmd, snapshot, fallback *os.File) {
	if len(c.ExtraFiles) < 3 || c.ExtraFiles[2] == nil || c.ProcessState == nil || c.ProcessState.ExitCode() != commandPipeBroken {
		return
	}
	if empty(snapshot) && empty(fallback) {
		s.commandArg.Store(true)
	}
}

// pipeCommand reports whether shells read the command from descriptor 5.
func (s *Session) pipeCommand() bool { return !s.commandArg.Load() }

// empty reports whether f holds nothing.
func empty(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Size() == 0
}

// commandArgs returns the positional parameters of a command's script: the
// fallback snapshot file and, unless piped, the command.
func commandArgs(fallback *os.File, cmd string, piped bool) []string {
	if piped {
		return []string{fallback.Name()}
	}
	return []string{fallback.Name(), cmd}
}

// releaseInputs closes the input files of a shell; command is nil unless
// piped.
func releaseInputs(replay, command *os.File) func() {
	return func() {
		proc.CloseLogged(replay)
		if command != nil {
			proc.CloseLogged(command)
		}
	}
}

// detached adds the desktop's git configuration and starts c in a new
// session. The session detaches the command from the CLI's controlling
// terminal, as the desktop does: sudo, ssh or git fail instead of stopping on
// SIGTTIN or writing over a permission prompt.
func detached(c *exec.Cmd) *exec.Cmd {
	c.Env = withGitConfig(c.Env)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return c
}

// newCmd runs shell sh with st's environment and env (State.environ).
// Running agent commands is the purpose of this tool; their lifetime is
// managed by killing the process group, not through a context.
//
// PWD names cwd as given, so that the shell's pwd keeps the symlinks of a root
// given through one (L2-1): bash and zsh take an inherited PWD only if it is
// the directory they start in. The desktop inherits the PWD of the app, which
// is the home directory, the default root, when it is started from there; a
// service has none.
func newCmd(sh shell, st State, cwd string, env []string, args ...string) *exec.Cmd {
	c := exec.Command(sh.path, args...) //nolint:gosec,noctx // see above
	c.Dir = cwd
	c.Env = st.environ(sh, env)
	if filepath.IsAbs(cwd) {
		c.Env = append(c.Env, "PWD="+cwd) // exec keeps the last of duplicates
	}
	return c
}

// inputFiles returns the replay script for st and, if piped, the command
// ended by a NUL, as the desktop writes it, for descriptors 3 and 5 of shell
// sh.
func (s *Session) inputFiles(sh shell, st State, cmd string, piped bool) (replay, command *os.File, err error) {
	if replay, err = s.tempFile("replay-", st.replayScript(sh)); err != nil {
		return nil, nil, err
	}
	if !piped {
		return replay, nil, nil
	}
	if command, err = s.tempFile("command-", cmd+"\x00"); err != nil {
		proc.CloseLogged(replay)
		return nil, nil, err
	}
	return replay, command, nil
}
