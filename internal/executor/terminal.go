package executor

import (
	"errors"
	"fmt"
	"os"

	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
)

// terminal creates the next free Root/terminals/<n>.txt output file.
func (e *Executor) terminal() (uint32, *os.File, error) {
	if err := e.Files.MkdirAll(terminalfile.Dir, 0o700); err != nil {
		return 0, nil, err
	}
	for {
		n := e.lastTerminal.Add(1)
		f, err := e.Files.OpenFile(fmt.Sprintf("%s/%d.txt", terminalfile.Dir, n), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		return n, f, err
	}
}
