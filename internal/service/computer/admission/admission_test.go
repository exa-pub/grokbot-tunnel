package admission_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/admission"
)

func exec(id string) *admission.Request {
	return &admission.Request{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{
		Exec: &aiserverv1.GrokBotUserComputerExec{ServerMessageJson: `{"readArgs":{"path":"x"}}`},
	}}
}

func cancel(id string) *admission.Request {
	return &admission.Request{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Cancel{
		Cancel: &aiserverv1.GrokBotUserComputerCancel{},
	}}
}

func putURL(id string) *admission.Request {
	return &admission.Request{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_PutFileToUrl{
		PutFileToUrl: &aiserverv1.GrokBotUserComputerPutFileToUrl{},
	}}
}

// full returns a Scheduler with MaxActive running and MaxBacklog waiting.
func full(t *testing.T) *admission.Scheduler {
	t.Helper()
	s := admission.New()
	for i := range admission.MaxActive + admission.MaxBacklog {
		v, err := s.Accept(exec(fmt.Sprint(i)), false)
		require.NoError(t, err)
		require.Equal(t, admission.Queued, v)
		if i < admission.MaxActive {
			run, stopped := s.Ready(true, false)
			require.Len(t, run, 1)
			require.Empty(t, stopped)
		}
	}
	require.Equal(t, admission.MaxActive, s.Active())
	require.Equal(t, admission.MaxBacklog, s.Backlog())
	return s
}

// Invariant 3: 8 running and 64 waiting; one more is refused in the
// desktop's words, and its redelivery is not answered again.
func TestAcceptBusy(t *testing.T) {
	s := full(t)
	v, err := s.Accept(exec("over"), false)
	assert.Equal(t, admission.Reject, v)
	require.ErrorIs(t, err, admission.ErrBusy)
	assert.Equal(t, "The user's computer is busy with other requests right now; try again shortly.", err.Error())
	v, _ = s.Accept(exec("over"), false)
	assert.Equal(t, admission.Ignore, v)
	assert.Len(t, s.Tasks(), admission.MaxActive+admission.MaxBacklog)
}

// Invariant 4: unrecognized frames are answered before admission, even when
// busy; without a request ID they are not answered at all.
func TestAcceptUnrecognized(t *testing.T) {
	s := full(t)
	v, err := s.Accept(putURL("url"), false)
	assert.Equal(t, admission.Reject, v)
	require.ErrorIs(t, err, frames.ErrUnknownFrame)
	v, _ = s.Accept(putURL("url"), false)
	assert.Equal(t, admission.Ignore, v, "repeated delivery answered twice")
	v, _ = s.Accept(&admission.Request{}, false)
	assert.Equal(t, admission.Ignore, v, "frame without a request id answered")
}

func TestAcceptVerdicts(t *testing.T) {
	s := admission.New()
	retire := &admission.Request{RequestId: "r", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_RetireApproval{
		RetireApproval: &aiserverv1.GrokBotUserComputerRetireApproval{},
	}}
	for _, tc := range []struct {
		f    *admission.Request
		want admission.Verdict
	}{
		{exec("a"), admission.Queued},
		{exec("a"), admission.Ignore},
		{cancel("a"), admission.Cancel},
		{retire, admission.Ignore},
	} {
		v, err := s.Accept(tc.f, false)
		require.NoError(t, err)
		assert.Equal(t, tc.want, v, tc.f.String())
	}
}

// Invariant 5: a cancel removes the request from the backlog, and a later
// delivery does not start it again.
func TestCancelWaitingRequest(t *testing.T) {
	s := full(t)
	t0 := s.Cancel(fmt.Sprint(admission.MaxActive)) // the first waiting one
	require.NotNil(t, t0)
	assert.Nil(t, t0.Cancel, "not started")
	assert.Equal(t, admission.MaxBacklog-1, s.Backlog())
	assert.True(t, s.Canceled(t0.ID))
	assert.NotContains(t, s.Tasks(), t0.ID)
	v, _ := s.Accept(t0.Frame, false)
	assert.Equal(t, admission.Ignore, v, "canceled work restarted")
	assert.Nil(t, s.Cancel("unknown"))
}

// Like the desktop's poll loop, a work frame that follows the cancel of its
// request in the same poll is answered as canceled before it started, once,
// and an unrecognized one as unknown; in a later poll, or for a request seen
// before the cancel, it is ignored.
func TestCancelBeforeWorkFrame(t *testing.T) {
	poll := func(s *admission.Scheduler, fs ...*admission.Request) []admission.Verdict {
		queued := make([]*aiserverv1.GrokBotUserComputerQueuedRequest, 0, len(fs))
		for _, f := range fs {
			queued = append(queued, &aiserverv1.GrokBotUserComputerQueuedRequest{Id: fmt.Sprint(f.RequestId, len(queued), f.Frame), Frame: f})
		}
		fresh := s.Polled(queued)
		out := make([]admission.Verdict, 0, len(fresh))
		for _, f := range fresh {
			v, err := s.Accept(f, false)
			if v == admission.Reject {
				want := frames.ErrCanceledBeforeStart
				if frames.Unrecognized(f) {
					want = frames.ErrUnknownFrame
				}
				require.ErrorIs(t, err, want)
			}
			if v == admission.Cancel {
				s.Cancel(f.RequestId)
			}
			out = append(out, v)
		}
		return out
	}
	R, C, I, Q := admission.Reject, admission.Cancel, admission.Ignore, admission.Queued
	for _, tc := range []struct {
		name  string
		polls [][]*admission.Request
		want  [][]admission.Verdict
	}{
		{"same poll", [][]*admission.Request{{cancel("a"), exec("a"), exec("a")}}, [][]admission.Verdict{{C, R, I}}},
		{"later poll", [][]*admission.Request{{cancel("a")}, {exec("a")}}, [][]admission.Verdict{{C}, {I}}},
		{"seen before", [][]*admission.Request{{exec("a"), cancel("a"), exec("a")}}, [][]admission.Verdict{{Q, C, I}}},
		{"unrecognized", [][]*admission.Request{{cancel("u"), putURL("u")}}, [][]admission.Verdict{{C, R}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := admission.New()
			for i, fs := range tc.polls {
				assert.Equal(t, tc.want[i], poll(s, fs...), i)
			}
			assert.Empty(t, s.Tasks())
		})
	}
}

// A started task stays until Finish; the caller stops it.
func TestCancelStartedRequest(t *testing.T) {
	s := admission.New()
	_, _ = s.Accept(exec("run"), false)
	run, _ := s.Ready(true, false)
	require.Len(t, run, 1)
	got := s.Cancel("run")
	assert.Same(t, run[0], got)
	assert.Equal(t, map[string]bool{"run": true}, s.Tasks())
	s.Finish("run")
	assert.Empty(t, s.Tasks())
	assert.Zero(t, s.Active())
}

// A request runs only under the server mode and with a credential it was
// accepted with; otherwise it is stopped, never run under another policy.
func TestReadyStopsRequestsOfAnotherMode(t *testing.T) {
	for _, tc := range []struct {
		name           string
		usable, server bool
	}{
		{"credential dropped", false, true},
		{"mode changed", true, false},
	} {
		s := admission.New()
		_, _ = s.Accept(exec("r"), true)
		run, stopped := s.Ready(tc.usable, tc.server)
		assert.Empty(t, run, tc.name)
		require.Len(t, stopped, 1, tc.name)
		assert.Equal(t, "r", stopped[0].ID)
		assert.Empty(t, s.Tasks(), tc.name)
		assert.Zero(t, s.Active(), tc.name)
	}
}

// Ready honors MaxActive and resumes after Finish.
func TestReadyLimit(t *testing.T) {
	s := admission.New()
	for i := range admission.MaxActive + 2 {
		_, _ = s.Accept(exec(fmt.Sprint(i)), false)
	}
	run, _ := s.Ready(true, false)
	assert.Len(t, run, admission.MaxActive)
	run, _ = s.Ready(true, false)
	assert.Empty(t, run)
	s.Finish("0")
	run, _ = s.Ready(true, false)
	require.Len(t, run, 1)
	assert.Equal(t, fmt.Sprint(admission.MaxActive), run[0].ID)
}

// Drop forgets the unstarted tasks for shutdown; their IDs stay in the
// backlog, where Ready skips them.
func TestDrop(t *testing.T) {
	s := full(t)
	dropped := s.Drop()
	assert.Len(t, dropped, admission.MaxBacklog)
	assert.Len(t, s.Tasks(), admission.MaxActive)
	assert.Equal(t, admission.MaxBacklog, s.Backlog())
	for i := range admission.MaxActive {
		s.Finish(fmt.Sprint(i))
	}
	run, stopped := s.Ready(true, false)
	assert.Empty(t, run)
	assert.Empty(t, stopped)
}

func TestPolledAcksAndDedup(t *testing.T) {
	s := admission.New()
	queued := []*aiserverv1.GrokBotUserComputerQueuedRequest{
		{Id: "q1", Frame: exec("a")},
		{Id: "q2"},
		{Id: "q1", Frame: exec("a")},
	}
	fresh := s.Polled(queued)
	assert.Equal(t, []string{"q1", "q2", "q1"}, s.Acks())
	require.Len(t, fresh, 1)
	assert.Equal(t, "a", fresh[0].RequestId)
	assert.Empty(t, s.Polled(queued[:1]), "queue id seen before")
	assert.Equal(t, []string{"q1"}, s.Acks())
	s.Polled(nil)
	assert.Empty(t, s.Acks())
}

// Queue IDs are remembered for the last 4096 only.
func TestPolledDedupWindow(t *testing.T) {
	s := admission.New()
	for i := range 4097 {
		s.Polled([]*aiserverv1.GrokBotUserComputerQueuedRequest{{Id: fmt.Sprint(i), Frame: exec(fmt.Sprint("r", i))}})
	}
	assert.Len(t, s.Polled([]*aiserverv1.GrokBotUserComputerQueuedRequest{{Id: "0", Frame: exec("new")}}), 1)
	assert.Empty(t, s.Polled([]*aiserverv1.GrokBotUserComputerQueuedRequest{{Id: "4096", Frame: exec("new2")}}))
}

func TestPoller(t *testing.T) {
	var p admission.Poller
	now := time.Unix(1000, 0)
	assert.False(t, p.Start(now), "no poll requested")
	p.Request()
	assert.True(t, p.Needed())
	assert.True(t, p.Start(now))
	assert.True(t, p.Polling())
	p.Request()
	assert.False(t, p.Start(now), "one poll in flight")
	// Failures back off 1 s, 2 s, 4 s.
	p.Done(now, assert.AnError, false)
	for _, wait := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		assert.False(t, p.Start(now.Add(wait-time.Millisecond)), wait)
		now = now.Add(wait)
		require.True(t, p.Start(now), wait)
		p.Done(now, assert.AnError, false)
	}
	now = now.Add(8 * time.Second)
	require.True(t, p.Start(now))
	p.Done(now, nil, true)
	assert.True(t, p.Start(now), "acks delivered right away, backoff reset")
	p.Done(now, nil, false)
	assert.False(t, p.Needed())
}

// Reset drops the backoff of failed polls: a channel that came back is
// polled at once, and the next failure backs off from 1 s again.
func TestPollerReset(t *testing.T) {
	var p admission.Poller
	now := time.Unix(1000, 0)
	for range 6 {
		p.Request()
		now = now.Add(time.Minute)
		require.True(t, p.Start(now))
		p.Done(now, assert.AnError, false)
	}
	require.False(t, p.Start(now.Add(30*time.Second)), "backoff below 32 s")
	p.Reset()
	assert.True(t, p.Needed())
	require.True(t, p.Start(now), "reset poller held back")
	p.Done(now, assert.AnError, false)
	assert.False(t, p.Start(now.Add(time.Second-time.Millisecond)))
	assert.True(t, p.Start(now.Add(time.Second)), "backoff not reset")
}
