package execstream_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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
		// L6: like the desktop's serializers, the server's execId is not
		// echoed.
		assert.Empty(t, result.ExecId)
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
	assert.True(t, proto.Equal(&agentv1.ExecClientThrow{Id: 1, Error: "boom", StackTrace: proto.String("Error: boom")},
		control(t, r.frames[0]).GetThrow()))
	assert.Equal(t, "missing", r.frames[0].GetControl().GetCwdState())
}

// L4, L5: like the desktop, an unsupported request throws the desktop's text
// without a stack trace, with the cwd_state of every throw (flt: "unset" for
// a request other than a shell), and still closes the stream.
func TestRunUnsupportedThrowsAndCloses(t *testing.T) {
	var m execstream.Manager
	var r recorder
	err := m.Run("req", &agentv1.ExecServerMessage{}, r.sink, func(execstream.Send) error {
		return &execstream.UnsupportedError{Case: "writeArgs"}
	}, noCwd)
	require.NoError(t, err)
	require.Len(t, r.frames, 2)
	throw := control(t, r.frames[0]).GetThrow()
	assert.Equal(t, "No handler found for server message of type writeArgs", throw.GetError())
	assert.Nil(t, throw.StackTrace)
	assert.Equal(t, "unset", r.frames[0].GetControl().GetCwdState())
	assert.NotNil(t, control(t, r.frames[1]).GetStreamClose())
	assert.Nil(t, r.frames[1].GetControl().CwdState)
}

// L5: the desktop's texts for unsupported requests (GGt).
func TestUnsupportedTexts(t *testing.T) {
	for c, want := range map[string]string{
		"":         "No handler found for server message of type undefined",
		"grepArgs": "No handler found for server message of type grepArgs",
		"computerUseArgs": "Computer use is not enabled for this environment, so the Computer tool and the computerUse subagent " +
			"cannot run here. Do not retry computer actions; verify with non-GUI methods (automated tests, shell commands) instead. " +
			"(No computer-use handler is registered on this exec daemon.)",
		"recordScreenArgs": "Screen recording is not enabled for this environment, so recordings cannot be captured here. " +
			"Do not retry; skip the recording and present other evidence instead. " +
			"(No record-screen handler is registered on this exec daemon.)",
	} {
		err := &execstream.UnsupportedError{Case: c}
		assert.Equal(t, want, err.Error(), c)
		assert.ErrorIs(t, err, execstream.ErrUnsupported)
	}
}

// L1: a heartbeat waiting for room in the queue does not hold up the
// results of its request.
func TestHeartbeatWaitDoesNotBlockResults(t *testing.T) {
	execstream.SetHeartbeatInterval(t, 10*time.Millisecond)
	var m execstream.Manager
	waiting := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	var results int
	sink := func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
		if f.GetControl() != nil && strings.Contains(f.GetControl().GetMessageJson(), "heartbeat") {
			once.Do(func() { close(waiting) })
			<-release
			return nil
		}
		if f.GetClient() != nil {
			mu.Lock()
			results++
			mu.Unlock()
		}
		return nil
	}
	sent := make(chan error, 1)
	go func() {
		sent <- m.Run("req", &agentv1.ExecServerMessage{}, sink, func(send execstream.Send) error {
			<-waiting
			done := make(chan error, 1)
			go func() { done <- send(readMessage("ok")) }()
			select {
			case err := <-done:
				close(release)
				return err
			case <-time.After(5 * time.Second):
				close(release)
				return errors.New("result waited behind the heartbeat")
			}
		}, noCwd)
	}()
	require.NoError(t, <-sent)
	assert.Equal(t, 1, results)
}

// L2: a frame the sink drops because the request was canceled does not stop
// the stream: the final frame is still sent, here a streamClose, as the
// tool failed only because of the drop.
func TestDropDoesNotStopStream(t *testing.T) {
	var m execstream.Manager
	var r recorder
	sink := func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
		if f.GetClient() != nil {
			return frames.Dropped(context.Canceled)
		}
		return r.sink(f)
	}
	err := m.Run("req", &agentv1.ExecServerMessage{}, sink, func(send execstream.Send) error {
		err := send(readMessage("a"))
		require.ErrorIs(t, err, frames.ErrDropped)
		return send(readMessage("b"))
	}, noCwd)
	require.NoError(t, err)
	require.Len(t, r.frames, 1)
	assert.NotNil(t, control(t, r.frames[0]).GetStreamClose())

	// A failure of the tool itself still throws after a drop.
	r = recorder{}
	err = m.Run("req", &agentv1.ExecServerMessage{}, sink, func(send execstream.Send) error {
		_ = send(readMessage("a"))
		return errors.New("boom")
	}, noCwd)
	require.NoError(t, err)
	require.Len(t, r.frames, 1)
	assert.Equal(t, "boom", control(t, r.frames[0]).GetThrow().GetError())
}

// L2: a dropped heartbeat does not stop the stream either.
func TestDroppedHeartbeatDoesNotStopStream(t *testing.T) {
	execstream.SetHeartbeatInterval(t, time.Millisecond)
	var m execstream.Manager
	var mu sync.Mutex
	var out []*aiserverv1.GrokBotUserComputerResponseFrame
	dropped := make(chan struct{})
	var once sync.Once
	sink := func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
		if f.GetControl() != nil && strings.Contains(f.GetControl().GetMessageJson(), "heartbeat") {
			once.Do(func() { close(dropped) })
			return frames.Dropped(context.Canceled)
		}
		mu.Lock()
		defer mu.Unlock()
		out = append(out, f)
		return nil
	}
	err := m.Run("req", &agentv1.ExecServerMessage{}, sink, func(send execstream.Send) error {
		<-dropped
		return send(readMessage("ok"))
	}, noCwd)
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.NotNil(t, out[0].GetClient())
	assert.NotNil(t, control(t, out[1]).GetStreamClose())
}

// L7: like the desktop, the execution time is rounded, not truncated.
func TestRoundMs(t *testing.T) {
	assert.Equal(t, int32(2), execstream.RoundMs(1500*time.Microsecond))
	assert.Equal(t, int32(1), execstream.RoundMs(1499*time.Microsecond))
	assert.Equal(t, int32(0), execstream.RoundMs(-time.Millisecond))
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
