package frames

import (
	"context"

	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

// Final reports whether f is the last frame of a request other than an exec:
// an error, a tool result or the last file chunk.
func Final(f *aiserverv1.GrokBotUserComputerResponseFrame) bool {
	return f.GetFileError() != nil || f.GetMessagesError() != nil || f.GetLocalToolResult() != nil || f.GetFile().GetLast()
}

// Closes reports whether f ends its request's stream.
func Closes(f *aiserverv1.GrokBotUserComputerResponseFrame) bool {
	if Final(f) {
		return true
	}
	if f.GetControl() == nil {
		return false
	}
	var msg agentv1.ExecClientControlMessage
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(f.GetControl().MessageJson), &msg); err != nil {
		return false
	}
	return msg.GetStreamClose() != nil || msg.GetThrow() != nil
}

// Keep reports whether f is still delivered after its request was canceled:
// control messages and final frames.
func Keep(f *aiserverv1.GrokBotUserComputerResponseFrame) bool {
	return f.GetControl() != nil || Final(f)
}

// CanceledFile replaces the last chunk of a canceled transfer, whose earlier
// chunks were dropped, with the error a canceled request reports.
func CanceledFile(f *aiserverv1.GrokBotUserComputerResponseFrame) *aiserverv1.GrokBotUserComputerResponseFrame {
	if f.GetFile() == nil {
		return f
	}
	return &aiserverv1.GrokBotUserComputerResponseFrame{
		RequestId: f.RequestId,
		Frame: &aiserverv1.GrokBotUserComputerResponseFrame_FileError{
			FileError: &aiserverv1.GrokBotUserComputerFileError{Error: context.Canceled.Error()},
		},
	}
}
