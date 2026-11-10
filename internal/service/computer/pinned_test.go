package computer

// Characterization tests from phase 0 of the refactor: they pin the observable
// behavior of the session, including where it knowingly differs from the
// desktop. The invariant numbers only tell the pinned behaviors apart. Invariant 11 (which
// requests are kept alive, and when) is pinned by replies.TestKeepAlive and
// TestKeepAliveSubmit.

import (
	"fmt"
	"testing"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/admission"
)

func pinnedExec(id string) *requestFrame {
	return &requestFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{
		Exec: &aiserverv1.GrokBotUserComputerExec{ServerMessageJson: `{"readArgs":{"path":"x"}}`},
	}}
}

func pinnedUpload(id string) *requestFrame {
	return &requestFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Upload{
		Upload: &aiserverv1.GrokBotUserComputerUpload{Path: "x", Data: []byte("data")},
	}}
}

func pinnedDownload(id string) *requestFrame {
	return &requestFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Download{
		Download: &aiserverv1.GrokBotUserComputerDownload{Path: "x"},
	}}
}

func pinnedMessagesOp(id string) *requestFrame {
	return &requestFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_MessagesOp{
		MessagesOp: &aiserverv1.GrokBotUserComputerMessagesOp{OpJson: `{"kind":"check-permissions"}`},
	}}
}

func pinnedLocalTool(id string) *requestFrame {
	return &requestFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_LocalTool{
		LocalTool: &aiserverv1.GrokBotUserComputerLocalToolCall{},
	}}
}

func pinnedClient(id string) *responseFrame {
	return &responseFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{
		Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: "result"},
	}}
}

// pinnedControl decodes the exec control message carried by f.
func pinnedControl(t *testing.T, f *responseFrame) *agentv1.ExecClientControlMessage {
	t.Helper()
	require.NotNil(t, f.GetControl(), "not a control frame: %v", f)
	var msg agentv1.ExecClientControlMessage
	require.NoError(t, protojson.Unmarshal([]byte(f.GetControl().GetMessageJson()), &msg))
	return &msg
}

// pinnedFrames returns the pending frames of request id.
func pinnedFrames(frames []*responseFrame, id string) []*responseFrame {
	var out []*responseFrame
	for _, f := range frames {
		if f.RequestId == id {
			out = append(out, f)
		}
	}
	return out
}

// Invariant 3 (admission): with maxActive requests running and the backlog of
// maxBacklog full, one more request is answered at once with the desktop's
// busy text (f4e), in the frame its kind uses for errors, and is not queued.
func TestPinnedBusyResponse(t *testing.T) {
	const busy = "The user's computer is busy with other requests right now; try again shortly."
	require.Equal(t, 8, admission.MaxActive)
	require.Equal(t, 64, admission.MaxBacklog)
	for _, tc := range []struct {
		name  string
		frame *requestFrame
		check func(t *testing.T, f *responseFrame)
	}{
		{"exec", pinnedExec("extra"), func(t *testing.T, f *responseFrame) {
			t.Helper()
			assert.Nil(t, f.GetControl().CwdState)
			msg := pinnedControl(t, f)
			require.NotNil(t, msg.GetThrow(), "want throw, got %v", msg)
			assert.Zero(t, msg.GetThrow().GetId())
			assert.Equal(t, busy, msg.GetThrow().GetError())
		}},
		{"upload", pinnedUpload("extra"), func(t *testing.T, f *responseFrame) {
			t.Helper()
			require.NotNil(t, f.GetFileError(), "want fileError, got %v", f)
			assert.Equal(t, busy, f.GetFileError().GetError())
		}},
		{"download", pinnedDownload("extra"), func(t *testing.T, f *responseFrame) {
			t.Helper()
			require.NotNil(t, f.GetFileError(), "want fileError, got %v", f)
			assert.Equal(t, busy, f.GetFileError().GetError())
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testSession(t)
			withCredential(s, "c", false)
			occupy(t, s, "busy", admission.MaxActive)
			for i := range admission.MaxBacklog {
				s.accept(pinnedExec(fmt.Sprint(i)), false)
			}
			require.Equal(t, admission.MaxBacklog, s.admit.Backlog())
			require.Empty(t, s.out.Pending())

			s.accept(tc.frame, false)
			require.Len(t, s.out.Pending(), 1)
			assert.Equal(t, "extra", s.out.Pending()[0].RequestId)
			tc.check(t, s.out.Pending()[0])
			assert.NotContains(t, s.admit.Tasks(), "extra")
			assert.Equal(t, admission.MaxBacklog, s.admit.Backlog())

			// A redelivery of the refused request is answered again, as
			// on the desktop.
			s.accept(tc.frame, false)
			assert.Len(t, s.out.Pending(), 2)
		})
	}
}

// Invariants 6 and 7 (D7, aligned with the desktop): a request that waits in
// the backlog and is canceled by the server gets "Cancel(l)ed before it
// started on the user's computer." (yae), on shutdown "…stopped serving
// requests…" (o7): an exec as throw{id:0}, other kinds in their error frame.
// The desktop sends streamClose{0} only when the cancel lands inside
// withInflight (localexec.Handle).
func TestPinnedCancelBeforeStart(t *testing.T) {
	cases := []struct {
		name  string
		frame func(id string) *requestFrame
		check func(t *testing.T, f *responseFrame, text string)
	}{
		{"exec", pinnedExec, func(t *testing.T, f *responseFrame, text string) {
			t.Helper()
			msg := pinnedControl(t, f)
			require.NotNil(t, msg.GetThrow(), "want throw, got %v", msg)
			assert.Zero(t, msg.GetThrow().GetId())
			assert.Equal(t, text, msg.GetThrow().GetError())
		}},
		{"upload", pinnedUpload, func(t *testing.T, f *responseFrame, text string) {
			t.Helper()
			require.NotNil(t, f.GetFileError(), "want fileError, got %v", f)
			assert.Equal(t, text, f.GetFileError().GetError())
		}},
		{"download", pinnedDownload, func(t *testing.T, f *responseFrame, text string) {
			t.Helper()
			require.NotNil(t, f.GetFileError(), "want fileError, got %v", f)
			assert.Equal(t, text, f.GetFileError().GetError())
		}},
		{"messagesOp", pinnedMessagesOp, func(t *testing.T, f *responseFrame, text string) {
			t.Helper()
			require.NotNil(t, f.GetMessagesError(), "want messagesError, got %v", f)
			assert.Equal(t, text, f.GetMessagesError().GetError())
		}},
		{"localTool", pinnedLocalTool, func(t *testing.T, f *responseFrame, text string) {
			t.Helper()
			require.NotNil(t, f.GetLocalToolResult(), "want localToolResult, got %v", f)
			assert.Equal(t, "failed", f.GetLocalToolResult().GetStatus())
			assert.Equal(t, text, f.GetLocalToolResult().GetError())
		}},
	}
	triggers := []struct {
		name    string
		cancel  func(s *session)
		text    string
		backlog int // IDs left in the backlog afterwards
	}{
		{"cancel frame", func(s *session) {
			s.accept(&requestFrame{RequestId: "r", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Cancel{
				Cancel: &aiserverv1.GrokBotUserComputerCancel{},
			}}, false)
		}, frames.ErrCanceledBeforeStart.Error(), 0},
		// Without a credential shutdown queues the frames but has nothing to
		// submit them with, so they stay pending for inspection. It drops the
		// task but leaves its id in the backlog, where startJobs would skip it.
		{"shutdown", func(s *session) {
			s.cred.Restore(entity.Credential{})
			s.shutdown()
		}, admission.ErrStopped.Error(), 1},
	}
	for _, tr := range triggers {
		for _, tc := range cases {
			t.Run(tr.name+"/"+tc.name, func(t *testing.T) {
				s := testSession(t)
				withCredential(s, "c", false)
				occupy(t, s, "busy", admission.MaxActive)
				s.accept(tc.frame("r"), false)
				require.Equal(t, 1, s.admit.Backlog())
				require.False(t, s.admit.Tasks()["r"], "task started")

				tr.cancel(s)
				assert.Equal(t, tr.backlog, s.admit.Backlog())
				assert.NotContains(t, s.admit.Tasks(), "r")
				require.Len(t, s.out.Pending(), 1)
				assert.Equal(t, "r", s.out.Pending()[0].RequestId)
				tc.check(t, s.out.Pending()[0], tr.text)
			})
		}
	}
}

// Invariant 10: closing frames keep their 30/120/180 s budget after a failed
// round while the session runs (replies.TestClosingFramesRetryWithinBudget).
// Shutdown only has shutdownGrace, of which finalFlushGrace is kept
// for the final submit: workers get shutdownGrace-finalFlushGrace to report,
// then everything already produced is flushed in one final submit bounded by
// the rest of shutdownGrace. This pins the budgets and the fast path: frames
// already pending, replies buffered but not yet taken, the throw of a backlogged request and the closing frame a canceled worker produces
// during shutdown are all submitted, and shutdown returns without waiting
// out any grace. The budgets are constants with no injectable clock, so the
// slow path (a worker that never finishes is abandoned after 6 s and the
// final submit still happens) is covered only by the real-time
// TestShutdownFlushesDespiteSlowWorker.
func TestPinnedShutdownFinalFlush(t *testing.T) {
	assert.Equal(t, 8*time.Second, shutdownGrace)
	assert.Equal(t, 2*time.Second, finalFlushGrace)

	var now time.Time
	c := &stubCloud{}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)

	s.out.Add(closeFrame("finished"))
	require.NoError(t, s.sink(pinnedClient("buffered")))
	occupy(t, s, "busy", admission.MaxActive-1)
	// A started request whose worker answers its cancellation.
	occupy(t, s, "close-running", 1)
	s.accept(pinnedExec("queued"), false)
	require.Equal(t, 1, s.admit.Backlog())

	start := time.Now()
	s.shutdown()
	assert.Less(t, time.Since(start), finalFlushGrace, "shutdown waited for a grace")

	assert.Empty(t, s.out.Pending())
	assert.Empty(t, s.admit.Tasks())
	assert.Zero(t, s.admit.Active())
	c.mu.Lock()
	submitted := c.submitted
	c.mu.Unlock()
	require.Len(t, submitted, 4)
	for _, id := range []string{"finished", "close-running0"} {
		got := pinnedFrames(submitted, id)
		require.Len(t, got, 1, id)
		assert.NotNil(t, pinnedControl(t, got[0]).GetStreamClose(), id)
	}
	got := pinnedFrames(submitted, "buffered")
	require.Len(t, got, 1)
	assert.Equal(t, "result", got[0].GetClient().GetMessageJson())
	got = pinnedFrames(submitted, "queued")
	require.Len(t, got, 1)
	msg := pinnedControl(t, got[0])
	require.NotNil(t, msg.GetThrow())
	assert.Equal(t, admission.ErrStopped.Error(), msg.GetThrow().GetError())
}
