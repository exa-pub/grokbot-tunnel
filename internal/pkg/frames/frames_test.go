package frames_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
)

type (
	request  = aiserverv1.GrokBotUserComputerRequestFrame
	response = aiserverv1.GrokBotUserComputerResponseFrame
)

func collect(t *testing.T, send func(frames.Sink) error) *response {
	t.Helper()
	var out []*response
	require.NoError(t, send(func(f *response) error { out = append(out, f); return nil }))
	require.Len(t, out, 1)
	return out[0]
}

func control(t *testing.T, f *response) *agentv1.ExecClientControlMessage {
	t.Helper()
	require.NotNil(t, f.GetControl())
	msg := &agentv1.ExecClientControlMessage{}
	require.NoError(t, protojson.Unmarshal([]byte(f.GetControl().MessageJson), msg))
	return msg
}

func TestReject(t *testing.T) {
	fail := errors.New("no")
	exec := &request{RequestId: "e", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{Exec: &aiserverv1.GrokBotUserComputerExec{}}}
	f := collect(t, func(s frames.Sink) error { return frames.Reject(exec, fail, s) })
	assert.Equal(t, "e", f.RequestId)
	assert.True(t, proto.Equal(&agentv1.ExecClientThrow{Id: 0, Error: "no"}, control(t, f).GetThrow()))
	assert.Nil(t, f.GetControl().CwdState)

	messages := &request{RequestId: "m", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_MessagesOp{
		MessagesOp: &aiserverv1.GrokBotUserComputerMessagesOp{},
	}}
	assert.Equal(t, "no", collect(t, func(s frames.Sink) error { return frames.Reject(messages, fail, s) }).GetMessagesError().GetError())

	tool := &request{RequestId: "t", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_LocalTool{
		LocalTool: &aiserverv1.GrokBotUserComputerLocalToolCall{},
	}}
	result := collect(t, func(s frames.Sink) error { return frames.Reject(tool, fail, s) }).GetLocalToolResult()
	assert.Equal(t, "failed", result.GetStatus())
	assert.Equal(t, "no", result.GetError())

	upload := &request{RequestId: "u", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Upload{
		Upload: &aiserverv1.GrokBotUserComputerUpload{},
	}}
	assert.Equal(t, "no", collect(t, func(s frames.Sink) error { return frames.Reject(upload, fail, s) }).GetFileError().GetError())
}

// Invariant 6, ≠: see internal/computer TestPinnedCancelBeforeStart.
func TestCanceled(t *testing.T) {
	exec := &request{RequestId: "e", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{Exec: &aiserverv1.GrokBotUserComputerExec{}}}
	f := collect(t, func(s frames.Sink) error { return frames.Canceled(exec, s) })
	require.NotNil(t, control(t, f).GetStreamClose())
	assert.Zero(t, control(t, f).GetStreamClose().GetId())
	download := &request{RequestId: "d", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Download{
		Download: &aiserverv1.GrokBotUserComputerDownload{},
	}}
	assert.Equal(t, context.Canceled.Error(),
		collect(t, func(s frames.Sink) error { return frames.Canceled(download, s) }).GetFileError().GetError())
}

func TestSendControlCarriesCwdState(t *testing.T) {
	cwd := "missing"
	f := collect(t, func(s frames.Sink) error { return frames.SendControl("r", frames.Heartbeat(7), &cwd, s) })
	assert.Equal(t, uint32(7), control(t, f).GetHeartbeat().GetId())
	assert.Equal(t, "missing", f.GetControl().GetCwdState())
}

func TestClassification(t *testing.T) {
	var closeJSON, heartbeatJSON []byte
	closeJSON, err := protojson.Marshal(frames.Close(3))
	require.NoError(t, err)
	heartbeatJSON, err = protojson.Marshal(frames.Heartbeat(3))
	require.NoError(t, err)
	controlFrame := func(b []byte) *response {
		return &response{Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Control{
			Control: &aiserverv1.GrokBotUserComputerControlMessage{MessageJson: string(b)},
		}}
	}
	client := &response{Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{Client: &aiserverv1.GrokBotUserComputerClientMessage{}}}
	for _, tc := range []struct {
		name                string
		f                   *response
		final, closes, keep bool
	}{
		{"chunk", frames.File("r", []byte("x"), 0, false), false, false, false},
		{"last chunk", frames.File("r", nil, 1, true), true, true, true},
		{"file error", &response{Frame: &aiserverv1.GrokBotUserComputerResponseFrame_FileError{
			FileError: &aiserverv1.GrokBotUserComputerFileError{},
		}}, true, true, true},
		{"stream close", controlFrame(closeJSON), false, true, true},
		{"heartbeat", controlFrame(heartbeatJSON), false, false, true},
		{"client message", client, false, false, false},
	} {
		assert.Equal(t, tc.final, frames.Final(tc.f), tc.name)
		assert.Equal(t, tc.closes, frames.Closes(tc.f), tc.name)
		assert.Equal(t, tc.keep, frames.Keep(tc.f), tc.name)
	}
}

func TestCanceledFile(t *testing.T) {
	last := frames.File("r", []byte("x"), 2, true)
	got := frames.CanceledFile(last)
	assert.Equal(t, "r", got.RequestId)
	assert.Equal(t, context.Canceled.Error(), got.GetFileError().GetError())
	other := &response{RequestId: "r", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_FileError{
		FileError: &aiserverv1.GrokBotUserComputerFileError{},
	}}
	assert.Same(t, other, frames.CanceledFile(other))
}

func TestUnrecognized(t *testing.T) {
	discover := func(source string) *request {
		return &request{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool{
			DiscoverAiTool: &aiserverv1.GrokBotUserComputerDiscoverAiTool{Source: source},
		}}
	}
	assert.True(t, frames.Unrecognized(&request{}))
	assert.True(t, frames.Unrecognized(&request{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_PutFileToUrl{}}))
	assert.True(t, frames.Unrecognized(&request{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_GetFileFromUrl{}}))
	assert.True(t, frames.Unrecognized(discover("cursor")))
	assert.False(t, frames.Unrecognized(discover(frames.SourceCodex)))
	assert.False(t, frames.Unrecognized(discover(frames.SourceClaudeCode)))
	assert.False(t, frames.Unrecognized(&request{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{}}))
}
