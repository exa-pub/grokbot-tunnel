package execstream_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
)

type recorder struct {
	frames []*aiserverv1.GrokBotUserComputerResponseFrame
	fail   error
}

func (r *recorder) sink(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
	r.frames = append(r.frames, f)
	return r.fail
}

func control(t *testing.T, f *aiserverv1.GrokBotUserComputerResponseFrame) *agentv1.ExecClientControlMessage {
	t.Helper()
	require.NotNil(t, f.GetControl())
	msg := &agentv1.ExecClientControlMessage{}
	require.NoError(t, protojson.Unmarshal([]byte(f.GetControl().MessageJson), msg))
	return msg
}

func readMessage(content string) *agentv1.ExecClientMessage {
	return &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_ReadResult{ReadResult: &agentv1.ReadResult{
		Result: &agentv1.ReadResult_Success{Success: &agentv1.ReadSuccess{Output: &agentv1.ReadSuccess_Content{Content: content}}},
	}}}
}

func noCwd(*agentv1.ExecServerMessage) *string { return proto.String("unset") }

func TestRunNumbersResultsAndCloses(t *testing.T) {
	var m execstream.Manager
	for want := uint32(1); want <= 2; want++ {
		var r recorder
		msg := &agentv1.ExecServerMessage{Id: 999, ExecId: "external"}
		err := m.Run("req", msg, r.sink, func(send execstream.Send) error { return send(readMessage("ok")) }, noCwd)
		require.NoError(t, err)
		require.Len(t, r.frames, 2)
		result := &agentv1.ExecClientMessage{}
		require.NoError(t, protojson.Unmarshal([]byte(r.frames[0].GetClient().GetMessageJson()), result))
		assert.Equal(t, want, result.Id)
		assert.Equal(t, "external", result.ExecId)
		assert.NotNil(t, result.LocalExecutionTimeMs)
		assert.Equal(t, "req", r.frames[1].RequestId)
		assert.Equal(t, want, control(t, r.frames[1]).GetStreamClose().GetId())
		assert.Nil(t, r.frames[1].GetControl().CwdState)
	}
}

func TestRunThrowsWithCwdState(t *testing.T) {
	var m execstream.Manager
	var r recorder
	err := m.Run("req", &agentv1.ExecServerMessage{}, r.sink, func(execstream.Send) error { return errors.New("boom") },
		func(*agentv1.ExecServerMessage) *string { return proto.String("missing") })
	require.NoError(t, err)
	require.Len(t, r.frames, 1)
	assert.True(t, proto.Equal(&agentv1.ExecClientThrow{Id: 1, Error: "boom"}, control(t, r.frames[0]).GetThrow()))
	assert.Equal(t, "missing", r.frames[0].GetControl().GetCwdState())
}

// An unsupported request throws without cwd_state and still closes the
// stream, like the desktop.
func TestRunUnsupportedThrowsAndCloses(t *testing.T) {
	var m execstream.Manager
	var r recorder
	err := m.Run("req", &agentv1.ExecServerMessage{}, r.sink, func(execstream.Send) error { return execstream.ErrUnsupported }, noCwd)
	require.NoError(t, err)
	require.Len(t, r.frames, 2)
	assert.Equal(t, execstream.ErrUnsupported.Error(), control(t, r.frames[0]).GetThrow().GetError())
	assert.Nil(t, r.frames[0].GetControl().CwdState)
	assert.NotNil(t, control(t, r.frames[1]).GetStreamClose())
}

// After the sink fails nothing else is sent and the failure is returned.
func TestRunStopsAfterSinkFailure(t *testing.T) {
	var m execstream.Manager
	r := recorder{fail: errors.New("gone")}
	err := m.Run("req", &agentv1.ExecServerMessage{}, r.sink, func(send execstream.Send) error {
		require.Error(t, send(readMessage("a")))
		return send(readMessage("b"))
	}, noCwd)
	require.EqualError(t, err, "gone")
	assert.Len(t, r.frames, 1)
}

func TestMarshalTruncatesReadContent(t *testing.T) {
	msg := readMessage(strings.Repeat("\"", execstream.MaxJSON/2+10)) // escaping doubles the size
	b, err := execstream.Marshal(msg)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(b), execstream.MaxJSON)
	assert.True(t, msg.GetReadResult().GetSuccess().GetTruncated())

	ls := &agentv1.ExecClientMessage{Message: &agentv1.ExecClientMessage_LsResult{LsResult: &agentv1.LsResult{
		Result: &agentv1.LsResult_Error{Error: &agentv1.LsError{Error: strings.Repeat("x", execstream.MaxJSON)}},
	}}}
	_, err = execstream.Marshal(ls)
	require.EqualError(t, err, "result exceeds 8 MiB")
}

var _ frames.Sink = (&recorder{}).sink
