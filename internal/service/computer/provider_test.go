package computer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/staterepo"
)

var connectedEvent = &aiserverv1.WatchGrokBotUserComputerRequestsEvent{
	Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Connected{Connected: &aiserverv1.WatchGrokBotUserComputerConnected{}},
}

// Like the desktop's user-computer-data and user-computer-control deadlines,
// a submit gets 120 s and a poll or a submit on shutdown 10 s.
func TestRPCDeadlines(t *testing.T) {
	var now time.Time
	c := &stubCloud{}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)
	s.stream.Start(now, "always")
	s.out.Add(closeFrame("a"))
	s.poll.Request()
	require.NoError(t, tick(t, s))
	c.mu.Lock()
	submit, poll := c.submitTimeout, c.pollTimeout
	c.mu.Unlock()
	assert.Greater(t, submit, 110*time.Second)
	assert.LessOrEqual(t, submit, 120*time.Second)
	assert.Greater(t, poll, 5*time.Second)
	assert.LessOrEqual(t, poll, 10*time.Second)

	s.out.Add(closeFrame("b"))
	require.NoError(t, s.submit(context.Background()))
	c.mu.Lock()
	submit = c.submitTimeout
	c.mu.Unlock()
	assert.Greater(t, submit, 5*time.Second)
	assert.LessOrEqual(t, submit, 10*time.Second, "shutdown submit")
}

// After failed polls, a watch that connects, a dial and a new credential
// poll at once instead of waiting out the poll backoff, like the desktop,
// whose connected handler polls at once.
func TestChannelReturnPollsAtOnce(t *testing.T) {
	for _, tc := range []struct {
		name    string
		trigger func(s *session)
	}{
		{"connected", func(s *session) {
			s.onEvent(watched{generation: s.stream.Generation(), event: connectedEvent})
		}},
		{"dial", func(s *session) { s.stream.Stop() }},
		{"new credential", func(s *session) {
			c := entity.Credential{Credential: "new", ExpiresAtMS: s.pv.now().Add(time.Hour).UnixMilli()}
			require.NoError(t, s.onIssued(issued{credential: c}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var now time.Time
			c := &stubCloud{failPoll: true}
			s := stubSession(t, c, &now)
			withCredential(s, "c", false)
			s.stream.Start(now, "always")
			s.poll.Request()
			for _, wait := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second} {
				require.NoError(t, tick(t, s))
				now = now.Add(wait)
			}
			require.NoError(t, tick(t, s)) // the next poll waits 16 s
			_, _, polls := c.counts()
			require.Equal(t, 5, polls)
			now = now.Add(time.Second)
			require.NoError(t, tick(t, s))
			require.True(t, s.stream.Alive())
			c.mu.Lock()
			c.failPoll = false
			c.mu.Unlock()
			tc.trigger(s)
			require.NoError(t, tick(t, s))
			_, _, polls = c.counts()
			assert.Equal(t, 6, polls, "poll waited for the backoff")
		})
	}
}

// A removal ends the session only once this process got a credential, like
// the desktop's ket; a removal reported to the first issue is retried, since
// the registration that started this process may not be visible yet.
func TestRemovedAfterIssueEndsSession(t *testing.T) {
	var now time.Time
	c := &stubCloud{removed: -1}
	s := stubSession(t, c, &now)
	require.NoError(t, tick(t, s))
	now = now.Add(29 * time.Second)
	require.NoError(t, tick(t, s))
	issues, _, _ := c.counts()
	require.Equal(t, 1, issues)
	now = now.Add(time.Second)
	require.NoError(t, tick(t, s))
	issues, _, _ = c.counts()
	require.Equal(t, 2, issues, "removal not retried")

	c = &stubCloud{removed: 1, ttl: 4 * time.Minute}
	s = stubSession(t, c, &now)
	require.NoError(t, tick(t, s))
	require.Equal(t, "1", s.cred.Current().Credential)
	now = now.Add(3 * time.Minute) // past the renewal halfway
	err := tick(t, s)
	require.ErrorIs(t, err, ErrRemoved)
	assert.Contains(t, err.Error(), "grokbot-tunnel connect")
}

// Like the desktop, a smaller accepted_count is only logged: the batch is
// done with, and nothing is sent twice.
func TestPartialAcceptDropsTheRest(t *testing.T) {
	var now time.Time
	c := &stubCloud{accept: 1}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)
	s.out.Add(closeFrame("a"))
	s.out.Add(closeFrame("b"))
	s.out.Add(closeFrame("c"))
	s.startSubmit()
	require.NoError(t, settle(t, s))
	s.startSubmit()
	require.NoError(t, settle(t, s))
	_, submits, _ := c.counts()
	assert.Equal(t, 1, submits)
	assert.Empty(t, s.out.Pending())
}

// A work frame that follows the cancel of its request in the same poll is
// answered as canceled before it started, like the desktop's poll loop.
func TestCancelThenWorkInOnePoll(t *testing.T) {
	var now time.Time
	c := &stubCloud{}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)
	s.poll.Request()
	require.True(t, s.poll.Start(now))
	s.onPolled(polled{credential: "c", requests: []*aiserverv1.GrokBotUserComputerQueuedRequest{
		{Id: "q1", Frame: cancelFrame("r")},
		{Id: "q2", Frame: execFrame("r")},
	}})
	require.NoError(t, settle(t, s))
	assert.Zero(t, s.admit.Active())
	c.mu.Lock()
	defer c.mu.Unlock()
	got := pinnedFrames(c.submitted, "r")
	if len(got) == 0 {
		got = pinnedFrames(s.out.Pending(), "r")
	}
	require.Len(t, got, 1)
	msg := pinnedControl(t, got[0])
	require.NotNil(t, msg.GetThrow())
	assert.Equal(t, frames.ErrCanceledBeforeStart.Error(), msg.GetThrow().GetError())
}

// A fatal error stops the session like a cancellation: the presence is
// withdrawn with the current credential (and the last acks delivered).
func TestFatalErrorWithdrawsPresence(t *testing.T) {
	c := &stubCloud{removed: 1, ttl: 600 * time.Millisecond, connect: true}
	srv := stubServer(t, c)
	cl := rpc.NewGrokBotServiceClient(srv.Client(), srv.URL)
	end := make(chan struct{})
	defer close(end)
	pv := &Provider{
		User: cl, Channel: cl,
		Store:    &staterepo.Store{Dir: t.TempDir()},
		Machine:  entity.Machine{MachineID: "m"},
		Handler:  testHandler{end: end},
		Standing: func() entity.Mode { return entity.ModeAlways },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.ErrorIs(t, pv.Run(ctx), ErrRemoved)
	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Equal(t, []string{"1"}, c.withdrawn)
}

// OnConnected is called once, when the server first accepts the computer,
// not when the watch is merely dialed.
func TestOnConnectedOnce(t *testing.T) {
	s := testSession(t)
	calls := 0
	s.pv.OnConnected = func() { calls++ }
	gen := s.stream.Start(s.pv.now(), "always")
	s.onEvent(watched{generation: gen, event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent{
		Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Notify{Notify: &aiserverv1.WatchGrokBotUserComputerNotify{}},
	}})
	assert.Zero(t, calls, "called before connected")
	s.onEvent(watched{generation: gen, event: connectedEvent})
	assert.Equal(t, 1, calls)
	gen = s.stream.Start(s.pv.now(), "always")
	s.onEvent(watched{generation: gen, event: connectedEvent})
	assert.Equal(t, 1, calls, "called again on reconnect")
}

// A stream that goes silent after connecting is renewed: the idle
// connections are closed, like the desktop, which makes a new client then.
// A stream that stalls before connecting is only dropped.
func TestSilentStreamClosesIdleConnections(t *testing.T) {
	for _, tc := range []struct {
		name      string
		connected bool
		silence   time.Duration
		closed    int
	}{
		{"silent", true, 16 * time.Second, 1},
		{"stalled", false, 36 * time.Second, 0},
		{"alive", true, 10 * time.Second, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testSession(t) // nothing to submit or poll
			closed := 0
			s.pv.CloseIdle = func() { closed++ }
			withCredential(s, "c", false)
			now := s.pv.now()
			gen := s.stream.Start(now.Add(-tc.silence), "always")
			if tc.connected {
				s.stream.Event(now.Add(-tc.silence), gen, connectedEvent)
			}
			s.onTick()
			assert.Equal(t, tc.closed, closed)
			assert.Equal(t, tc.silence < 15*time.Second, s.stream.Alive())
		})
	}
}

// The hello names the computer like the desktop's computerLabel: its label,
// else the host name, trimmed, else "this computer".
func TestHelloLabel(t *testing.T) {
	defer func(h func() (string, error)) { hostname = h }(hostname)
	for _, tc := range []struct {
		label, host string
		hostErr     error
		want        string
	}{
		{"desk", "host", nil, "desk"},
		{"  desk \n", "host", nil, "desk"},
		{"", " host\n", nil, "host"},
		{"  ", "host", nil, "host"},
		{"", "  ", nil, "this computer"},
		{"", "host", errors.New("no host name"), "this computer"},
	} {
		hostname = func() (string, error) { return tc.host, tc.hostErr }
		pv := &Provider{Machine: entity.Machine{Label: tc.label}}
		assert.Equal(t, tc.want, pv.hello(entity.Credential{}, "").GetLabel(), "%q %q", tc.label, tc.host)
	}
}
