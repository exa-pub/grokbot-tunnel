package localexec

import (
	"context"
	"errors"
	"log"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
)

// request is a parsed request frame.
type request struct {
	msg      *agentv1.ExecServerMessage // empty unless the frame is an exec
	action   string                     // shown in the permission prompt
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
		if r.action = describe(r.msg); r.action == "" {
			r.action = "Execute unsupported local tool"
		}
		auth = v.Exec
	case *aiserverv1.GrokBotUserComputerRequestFrame_Upload:
		r.action = "Write " + v.Upload.Path
		auth = v.Upload
	case *aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool:
		r.action = "Read AI tool setup and recent history for " + v.DiscoverAiTool.Source +
			" in the home/config directories (may be outside root)"
		auth = v.DiscoverAiTool
	case *aiserverv1.GrokBotUserComputerRequestFrame_Download:
		r.action = "Read " + v.Download.Path
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
		log.Printf("request %s: %s", f.RequestId, req.action)
		err = e.gate(ctx, req.action, server, req.approved)
	}
	if err != nil {
		return frames.Reject(f, err, sink)
	}
	if ctx.Err() != nil {
		return frames.Canceled(f, sink)
	}
	if f.GetExec() != nil {
		return e.controlled(ctx, f, req.msg, sink)
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
			return err
		}
		return sink(frames.File(f.RequestId, nil, 0, true))
	case *aiserverv1.GrokBotUserComputerRequestFrame_Download:
		return e.transfer.Download(ctx, f.RequestId, v.Download.Path, sink)
	case *aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool:
		data, err := e.discovery.Discover(ctx, v.DiscoverAiTool.Source)
		if err != nil {
			return err
		}
		return sink(frames.File(f.RequestId, data, 0, true))
	}
	return nil
}

func (e *Executor) exec(ctx context.Context, msg *agentv1.ExecServerMessage, send execstream.Send) error {
	switch {
	case msg.GetReadArgs() != nil:
		return e.reader.Execute(ctx, msg.GetReadArgs(), send)
	case msg.GetLsArgs() != nil:
		return e.lister.Execute(ctx, msg.GetLsArgs(), send)
	case msg.GetShellStreamArgs() != nil:
		return e.shell.Execute(ctx, msg.GetShellStreamArgs(), send)
	case msg.GetBackgroundShellSpawnArgs() != nil:
		return e.background.Execute(ctx, msg.GetBackgroundShellSpawnArgs(), send)
	}
	return execstream.ErrUnsupported
}

// controlled runs an exec request as a stream with an exec ID, heartbeats and
// a final throw or close.
func (e *Executor) controlled(
	ctx context.Context, f *aiserverv1.GrokBotUserComputerRequestFrame, msg *agentv1.ExecServerMessage, sink frames.Sink,
) error {
	return e.streams.Run(f.RequestId, msg, sink, func(send execstream.Send) error { return e.exec(ctx, msg, send) }, e.cwdState)
}

// cwdState describes the requested shell working directory for a failed
// request: "exists", "missing" or "unset"; nil if it cannot be determined.
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
