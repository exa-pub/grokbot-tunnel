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
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

const (
	// listBudget is how many UTF-16 code units of names a listing may hold.
	listBudget     = 2500
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
// fails the request instead of producing a result, as on the desktop.
func (ls *Executor) Execute(ctx context.Context, x *agentv1.LsArgs, send execstream.Send) error {
	rel, err := ls.root.Relative(x.Path)
	if err != nil {
		return err
	}
	path := ls.root.Abs(rel)
	fail := func(msg string) error {
		return send(lsMessage(&agentv1.LsResult{Result: &agentv1.LsResult_Error{
			Error: &agentv1.LsError{Path: path, Error: msg},
		}}))
	}
	st, err := ls.root.FS().Stat(rel)
	switch {
	case pathroot.Escaped(err):
		return err
	case errors.Is(err, os.ErrNotExist):
		return fail("Path does not exist: " + path)
	case err != nil:
		return fail(err.Error())
	case !st.IsDir():
		return fail("Path is not a directory: " + path)
	}
	timeout := defaultTimeout
	if x.TimeoutMs != nil {
		timeout = time.Duration(x.GetTimeoutMs()) * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	l := lister{root: ls.root.Dir(), files: ls.root.FS(), ignore: x.Ignore, budget: listBudget - nameCost(path)}
	tree, err := l.list(ctx, rel)
	validUTF8(tree)
	if errors.Is(err, context.DeadlineExceeded) {
		return send(lsMessage(&agentv1.LsResult{Result: &agentv1.LsResult_Timeout{
			Timeout: &agentv1.LsTimeout{DirectoryTreeRoot: tree},
		}}))
	}
	if err != nil {
		return fail(err.Error())
	}
	return send(lsMessage(&agentv1.LsResult{Result: &agentv1.LsResult_Success{
		Success: &agentv1.LsSuccess{DirectoryTreeRoot: tree},
	}}))
}
