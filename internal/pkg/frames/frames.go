// Package frames builds and classifies the frames of the user-computer
// channel: answers to request frames that never reach a tool, exec control
// messages and file chunks.
package frames

import (
	"context"
	"errors"

	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

// Sink delivers response frames to the server.
type Sink func(*aiserverv1.GrokBotUserComputerResponseFrame) error

// Reject answers f with err in the form the request type expects.
func Reject(f *aiserverv1.GrokBotUserComputerRequestFrame, err error, sink Sink) error {
	resp := &aiserverv1.GrokBotUserComputerResponseFrame{RequestId: f.RequestId}
	switch {
	case f.GetExec() != nil, f.Frame == nil, f.GetPutFileToUrl() != nil, f.GetGetFileFromUrl() != nil:
		return SendControl(f.RequestId, Throw(0, err), nil, sink)
	case f.GetMessagesOp() != nil:
		resp.Frame = &aiserverv1.GrokBotUserComputerResponseFrame_MessagesError{
			MessagesError: &aiserverv1.GrokBotUserComputerMessagesError{Error: err.Error()},
		}
	case f.GetLocalTool() != nil:
		resp.Frame = &aiserverv1.GrokBotUserComputerResponseFrame_LocalToolResult{
			LocalToolResult: &aiserverv1.GrokBotUserComputerLocalToolResult{Status: "failed", Error: err.Error()},
		}
	default:
		resp.Frame = &aiserverv1.GrokBotUserComputerResponseFrame_FileError{
			FileError: &aiserverv1.GrokBotUserComputerFileError{Error: err.Error()},
		}
	}
	return sink(resp)
}

// ErrCanceledBeforeStart answers a request canceled while it waited to run,
// in the desktop's words (yae).
//
//nolint:revive,staticcheck // user-facing sentence
var ErrCanceledBeforeStart = errors.New("Cancelled before it started on the user's computer.") //nolint:misspell // desktop text

// CanceledBeforeStart answers a request that the server canceled before it
// started, like the desktop: an exec throws, other requests get their error
// frame.
func CanceledBeforeStart(f *aiserverv1.GrokBotUserComputerRequestFrame, sink Sink) error {
	return Reject(f, ErrCanceledBeforeStart, sink)
}

// Canceled closes work canceled after it was admitted but before an exec ID
// was assigned, like the desktop inside withInflight: an exec gets
// streamClose{0}.
func Canceled(f *aiserverv1.GrokBotUserComputerRequestFrame, sink Sink) error {
	if f.GetExec() == nil {
		return Reject(f, context.Canceled, sink)
	}
	return SendControl(f.RequestId, Close(0), nil, sink)
}

// File returns a chunk of file data; seq counts from 0 and last marks the
// final chunk.
func File(requestID string, data []byte, seq uint32, last bool) *aiserverv1.GrokBotUserComputerResponseFrame {
	return &aiserverv1.GrokBotUserComputerResponseFrame{
		RequestId: requestID,
		Frame: &aiserverv1.GrokBotUserComputerResponseFrame_File{
			File: &aiserverv1.GrokBotUserComputerFile{Data: data, Seq: seq, Last: last},
		},
	}
}

// Throw ends exec execID with err. An err with a StackTrace() string method
// in its chain also fills the throw's stack trace.
func Throw(execID uint32, err error) *agentv1.ExecClientControlMessage {
	throw := &agentv1.ExecClientThrow{Id: execID, Error: err.Error()}
	var traced interface{ StackTrace() string }
	if errors.As(err, &traced) {
		trace := traced.StackTrace()
		throw.StackTrace = &trace
	}
	return &agentv1.ExecClientControlMessage{Message: &agentv1.ExecClientControlMessage_Throw{Throw: throw}}
}

// Close ends exec execID successfully.
func Close(execID uint32) *agentv1.ExecClientControlMessage {
	return &agentv1.ExecClientControlMessage{Message: &agentv1.ExecClientControlMessage_StreamClose{
		StreamClose: &agentv1.ExecClientStreamClose{Id: execID},
	}}
}

// Heartbeat tells the server that exec execID is still running.
func Heartbeat(execID uint32) *agentv1.ExecClientControlMessage {
	return &agentv1.ExecClientControlMessage{Message: &agentv1.ExecClientControlMessage_Heartbeat{
		Heartbeat: &agentv1.ExecClientHeartbeat{Id: execID},
	}}
}

// SendControl sends an exec control message; cwd optionally reports the
// state of the requested working directory.
func SendControl(requestID string, msg *agentv1.ExecClientControlMessage, cwd *string, sink Sink) error {
	b, err := protojson.Marshal(msg)
	if err != nil {
		return err
	}
	return sink(&aiserverv1.GrokBotUserComputerResponseFrame{
		RequestId: requestID,
		Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Control{
			Control: &aiserverv1.GrokBotUserComputerControlMessage{MessageJson: string(b), CwdState: cwd},
		},
	})
}
