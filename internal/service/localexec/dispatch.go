package localexec

import (
	"context"
	"errors"
	"fmt"
	"log"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

// request is a parsed request frame.
type request struct {
	msg      *agentv1.ExecServerMessage // empty unless the frame is an exec
	execCase string                     // the desktop's case of an exec, "" if none
	action   string                     // shown in the permission prompt; "" for an unsupported exec
	approved bool                       // authorized by the server
}

// authorization is implemented by every request type the server can authorize.
type authorization interface {
	GetAuthorizedByApproval() bool
	GetAuthorizedByStanding() bool
}

func parse(f *aiserverv1.GrokBotUserComputerRequestFrame) (request, error) {
	r := request{msg: &agentv1.ExecServerMessage{}}
	if frames.Unrecognized(f) {
		return r, frames.ErrUnknownFrame
	}
	var auth authorization
	switch v := f.Frame.(type) {
	case *aiserverv1.GrokBotUserComputerRequestFrame_Exec:
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(v.Exec.ServerMessageJson), r.msg); err != nil {
			return r, err
		}
		r.action = describe(r.msg)
		r.execCase = execCase(v.Exec.ServerMessageJson)
		auth = v.Exec
	case *aiserverv1.GrokBotUserComputerRequestFrame_Upload:
		r.action = fmt.Sprintf("Write %q", v.Upload.Path)
		auth = v.Upload
	case *aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool:
		r.action = fmt.Sprintf("Read AI tool setup and recent history for %q", v.DiscoverAiTool.Source) +
			" in the home/config directories (may be outside root)"
		auth = v.DiscoverAiTool
	case *aiserverv1.GrokBotUserComputerRequestFrame_Download:
		r.action = fmt.Sprintf("Read %q", v.Download.Path)
		auth = v.Download
	default:
		return r, errors.New("unsupported on Linux")
	}
	r.approved = auth.GetAuthorizedByApproval() || auth.GetAuthorizedByStanding()
	return r, nil
}

// Handle checks permission for f, runs it and streams the results to sink.
// Failures are reported to sink; the returned error means sink itself failed.
// server selects server-authoritative permission checks.
func (e *Executor) Handle(ctx context.Context, f *aiserverv1.GrokBotUserComputerRequestFrame, server bool, sink frames.Sink) error {
	switch f.Frame.(type) {
	case *aiserverv1.GrokBotUserComputerRequestFrame_Cancel, *aiserverv1.GrokBotUserComputerRequestFrame_RetireApproval:
		return nil // handled by the scheduler
	}
	defer log.Printf("request %s finished", f.RequestId)
	req, err := parse(f)
	if err == nil {
		if req.action != "" {
			log.Printf("request %s: %s", f.RequestId, req.action)
		} else {
			log.Printf("request %s: unsupported exec request %q", f.RequestId, req.execCase)
		}
		err = e.gate(ctx, req.action, server, req.approved)
		// Like the desktop, which checks the cancellation after the
		// permission check, a check that ended with the request's
		// cancellation closes the request instead of refusing it.
		if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return frames.Canceled(f, sink)
		}
	}
	if err != nil {
		return frames.Reject(f, err, sink)
	}
	if ctx.Err() != nil {
		return frames.Canceled(f, sink)
	}
	if f.GetExec() != nil {
		return e.controlled(ctx, f, req, sink)
	}
	if err = e.run(ctx, f, sink); err != nil {
		return frames.Reject(f, err, sink)
	}
	return nil
}

func (e *Executor) run(ctx context.Context, f *aiserverv1.GrokBotUserComputerRequestFrame, sink frames.Sink) error {
	switch v := f.Frame.(type) {
	case *aiserverv1.GrokBotUserComputerRequestFrame_Upload:
		if err := e.transfer.Upload(v.Upload.Path, v.Upload.Data); err != nil {
			return pathroot.Refuse(v.Upload.Path, err)
		}
		return sink(frames.File(f.RequestId, nil, 0, true))
	case *aiserverv1.GrokBotUserComputerRequestFrame_Download:
		return pathroot.Refuse(v.Download.Path, e.transfer.Download(ctx, f.RequestId, v.Download.Path, sink))
	case *aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool:
		data, err := e.discovery.Discover(ctx, v.DiscoverAiTool.Source)
		if err != nil {
			return err
		}
		return sink(frames.File(f.RequestId, data, 0, true))
	}
	return nil
}

func (e *Executor) exec(ctx context.Context, req request, send execstream.Send) error {
	msg := req.msg
	switch {
	case msg.GetReadArgs() != nil:
		return pathroot.Refuse(msg.GetReadArgs().Path, e.reader.Execute(ctx, msg.GetReadArgs(), send))
	case msg.GetLsArgs() != nil:
		return pathroot.Refuse(msg.GetLsArgs().Path, e.lister.Execute(ctx, msg.GetLsArgs(), send))
	case msg.GetShellStreamArgs() != nil:
		x := msg.GetShellStreamArgs()
		warning, err := e.resolveCwd(&x.WorkingDirectory)
		if err != nil {
			return err
		}
		if warning != "" {
			if err := send(stderrMessage(warning)); err != nil {
				return err
			}
		}
		return e.shell.Execute(ctx, x, send)
	case msg.GetBackgroundShellSpawnArgs() != nil:
		x := msg.GetBackgroundShellSpawnArgs()
		if _, err := e.resolveCwd(&x.WorkingDirectory); err != nil {
			return err
		}
		return e.background.Execute(ctx, x, send)
	}
	return &execstream.UnsupportedError{Case: req.execCase}
}

// resolveCwd replaces the requested working directory of a shell request
// with the one it runs in, like the desktop's dispatcher: "" for the
// session's cwd, an existing directory, or the root, for which it returns
// the warning a foreground command reports. A directory that cannot be
// checked fails the request. The cwd_state of a failure then describes the
// directory used.
func (e *Executor) resolveCwd(dir *string) (warning string, err error) {
	resolved, warning, err := e.root.ResolveCwd(*dir)
	if err != nil {
		return "", err
	}
	*dir = resolved
	return warning, nil
}

func stderrMessage(data string) *agentv1.ExecClientMessage {
	return &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ShellStream{ShellStream: &agentv1.ShellStream{
		Event: &agentv1.ShellStream_Stderr{Stderr: &agentv1.ShellStreamStderr{Data: data}},
	}}}
}

// controlled runs an exec request as a stream with an exec ID, heartbeats and
// a final throw or close.
func (e *Executor) controlled(ctx context.Context, f *aiserverv1.GrokBotUserComputerRequestFrame, req request, sink frames.Sink) error {
	return e.streams.Run(f.RequestId, req.msg, sink, func(send execstream.Send) error { return e.exec(ctx, req, send) }, e.cwdState)
}

// cwdState describes the working directory of a failed request, like the
// desktop's flt: "exists" or "missing" for a shell request, "unset" for any
// other; nil if it cannot be determined.
func (e *Executor) cwdState(msg *agentv1.ExecServerMessage) *string {
	var path string
	switch {
	case msg.GetShellStreamArgs() != nil:
		path = msg.GetShellStreamArgs().WorkingDirectory
	case msg.GetBackgroundShellSpawnArgs() != nil:
		path = msg.GetBackgroundShellSpawnArgs().WorkingDirectory
	default:
		return proto.String("unset")
	}
	return e.root.CwdState(path)
}
