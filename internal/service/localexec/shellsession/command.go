package shellsession

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
)

// Foreground returns the shell that runs cmd in cwd with st replayed and
// saves the state it leaves to snapshot, or from its EXIT trap to fallback.
// release closes the input files once the shell has started or failed to.
func Foreground(st State, cwd, cmd string, snapshot, fallback *os.File) (c *exec.Cmd, release func(), err error) {
	replay, command, err := inputFiles(st, cmd)
	if err != nil {
		return nil, nil, err
	}
	sh := userShell()
	c = newCmd(sh, st, cwd, sh.args("-c", sh.foregroundScript(), "grokbot-shell", fallback.Name())...)
	c.ExtraFiles = []*os.File{replay, snapshot, command}
	return detached(c), func() { proc.CloseLogged(replay); proc.CloseLogged(command) }, nil
}

// Background returns the shell that runs cmd in cwd with st replayed once a
// line is written to the other end of gate, and saves its state like
// Foreground. release closes the input files once the shell has started or
// failed to.
func Background(st State, cwd, cmd string, gate, snapshot, fallback *os.File) (c *exec.Cmd, release func(), err error) {
	replay, command, err := inputFiles(st, cmd)
	if err != nil {
		return nil, nil, err
	}
	sh := userShell()
	c = newCmd(sh, st, cwd, sh.args("-c", sh.backgroundScript(), "grok-background", fallback.Name())...)
	c.ExtraFiles = []*os.File{replay, snapshot, command, gate}
	return detached(c), func() { proc.CloseLogged(replay); proc.CloseLogged(command) }, nil
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

// newCmd runs the user's shell with st's environment. Running agent commands
// is the purpose of this tool; their lifetime is managed by killing the
// process group, not through a context.
func newCmd(sh shell, st State, cwd string, args ...string) *exec.Cmd {
	c := exec.Command(sh.path, args...) //nolint:gosec,noctx // see above
	c.Dir = cwd
	c.Env = st.environ()
	return c
}

// inputFiles returns the replay script for st and the command, for
// descriptors 3 and 5 of a shell.
func inputFiles(st State, cmd string) (replay, command *os.File, err error) {
	if replay, err = proc.TempFile("grok-shell-replay-", st.replayScript()); err != nil {
		return nil, nil, err
	}
	if command, err = proc.TempFile("grok-command-", cmd); err != nil {
		proc.CloseLogged(replay)
		return nil, nil, err
	}
	return replay, command, nil
}
