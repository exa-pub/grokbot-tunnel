package executor

import (
	"errors"
	"io"
	"os"
	"os/exec"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
)

// shellCommand runs the user's shell with st's environment. Running agent
// commands is the purpose of this tool; their lifetime is managed by killing
// the process group, not through a context.
func shellCommand(sh shell, st shellState, cwd string, args ...string) *exec.Cmd {
	c := exec.Command(sh.path, args...) //nolint:gosec,noctx // see above
	c.Dir = cwd
	c.Env = st.environ()
	return c
}

// inputFiles returns the replay script for st and the command, for
// descriptors 3 and 5 of a shell.
func inputFiles(st shellState, cmd string) (replay, command *os.File, err error) {
	if replay, err = proc.TempFile("grok-shell-replay-", st.replayScript()); err != nil {
		return nil, nil, err
	}
	if command, err = proc.TempFile("grok-command-", cmd); err != nil {
		proc.CloseLogged(replay)
		return nil, nil, err
	}
	return replay, command, nil
}

// readAtMost reads up to n bytes at off; a short read at the end of the file
// is not an error.
func readAtMost(f *os.File, n, off int64) ([]byte, error) {
	b := make([]byte, n)
	read, err := f.ReadAt(b, off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return b[:read], nil
}
