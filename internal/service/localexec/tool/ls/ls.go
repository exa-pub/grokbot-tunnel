// Package ls lists a directory tree for the agent, like the desktop's
// local-exec ls executor: ripgrep's file enumeration when rg is installed, a
// budget of 2500 UTF-16 code units of names, extension counts for the rest
// and a partial tree on timeout.
package ls

import (
	"context"
	"errors"
	"os"
	"time"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/nodeerr"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

const (
	defaultTimeout = 5 * time.Second
)

// Executor lists directories below a root.
type Executor struct {
	root *pathroot.Root
}

// New returns an Executor for directories below root.
func New(root *pathroot.Root) *Executor { return &Executor{root: root} }

func lsMessage(r *agentv1.LsResult) *agentv1.ExecClientMessage {
	return &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_LsResult{LsResult: r}}
}

// Execute lists args.Path and sends the ls result. A path outside the root
// fails the request instead of producing a result, as on the desktop; below
// a root directory that cannot be opened, the path does not exist.
func (ls *Executor) Execute(ctx context.Context, x *agentv1.LsArgs, send execstream.Send) error {
	p, err := ls.root.Check(x.Path)
	var unavailable *pathroot.RootError
	if errors.As(err, &unavailable) {
		return send(statError(unavailable.Path, unavailable.Err))
	}
	if err != nil {
		return err
	}
	defer p.Close()
	path := ls.root.Abs(p.Rel)
	st, err := p.FS().Stat(p.Real)
	switch {
	case pathroot.Escaped(err):
		return err
	case err != nil:
		return send(statError(path, err))
	case !st.IsDir():
		return send(lsError(path, "Path is not a directory: "+path))
	}
	timeout := defaultTimeout
	if x.TimeoutMs != nil {
		timeout = time.Duration(x.GetTimeoutMs()) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	l := lister{root: path, files: p.FS(), base: p.Real, ignore: x.Ignore}
	tree, err := l.list(ctx, ".")
	validUTF8(tree)
	if errors.Is(err, context.DeadlineExceeded) {
		return send(lsMessage(&agentv1.LsResult{Result: &agentv1.LsResult_Timeout{
			Timeout: &agentv1.LsTimeout{DirectoryTreeRoot: tree},
		}}))
	}
	if err != nil {
		return send(lsError(path, nodeerr.Wrap(err, path).Error()))
	}
	return send(lsMessage(&agentv1.LsResult{Result: &agentv1.LsResult_Success{
		Success: &agentv1.LsSuccess{DirectoryTreeRoot: tree},
	}}))
}

func lsError(path, msg string) *agentv1.ExecClientMessage {
	return lsMessage(&agentv1.LsResult{Result: &agentv1.LsResult_Error{
		Error: &agentv1.LsError{Path: path, Error: msg},
	}})
}

// statError is the result for a directory that cannot be listed by its
// stat.
func statError(path string, err error) *agentv1.ExecClientMessage {
	if errors.Is(err, os.ErrNotExist) {
		return lsError(path, "Path does not exist: "+path)
	}
	return lsError(path, nodeerr.Wrap(err, path).Error())
}
