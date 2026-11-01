// Package localexec runs the requests of the user-computer channel on this
// computer, like the desktop's local-exec daemon and its dispatcher (anchor:
// "No handler found for server message of type"): it parses a request frame,
// asks the permission gate, and hands the request to its tool module, exec
// requests through an exec stream. Shell commands run with the user's
// privileges; the root confines the file tools, not the shell.
package localexec

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/shellsession"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/background"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/discovery"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/ls"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/read"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/shell"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/transfer"
)

// Gate decides whether action may run, like the desktop's isLocalUseBlocked.
// server selects server-authoritative permission checks, in which approved is
// the server's decision. An empty action is an exec request that no tool
// handles, which the desktop does not describe; like the desktop, the gate
// does not ask the user about it. It returns nil to allow the action, or the
// error the agent sees.
type Gate func(ctx context.Context, action string, server, approved bool) error

// Executor runs user-computer requests below Root.
type Executor struct {
	Root string
	root *pathroot.Root
	gate Gate

	streams    execstream.Manager
	session    *shellsession.Session
	shell      *shell.Executor
	background *background.Executor
	reader     *read.Executor
	lister     *ls.Executor
	transfer   *transfer.Transfer
	discovery  *discovery.Discoverer
}

// New opens an executor rooted at root; like the desktop, symlinks in root
// itself are kept.
func New(root string, gate Gate) (*Executor, error) {
	r, err := pathroot.Open(root)
	if err != nil {
		return nil, err
	}
	session, err := shellsession.New(r)
	if err != nil {
		return nil, errors.Join(err, r.Close())
	}
	bg := background.New(r, session)
	return &Executor{
		Root:       r.Dir(),
		root:       r,
		gate:       gate,
		session:    session,
		shell:      shell.New(session, bg),
		background: bg,
		reader:     read.New(r),
		lister:     ls.New(r),
		transfer:   transfer.New(r),
		discovery:  discovery.New(discovery.Config{}),
	}, nil
}

// TerminalsFolder is where background command output files are written.
func (e *Executor) TerminalsFolder() string { return filepath.Join(e.Root, terminalfile.Dir) }

// Close removes the shells' temporary files and releases the root directory
// handle.
func (e *Executor) Close() error { return errors.Join(e.session.Close(), e.root.Close()) }
