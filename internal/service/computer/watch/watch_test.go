package watch_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/watch"
)

func noJitter(time.Duration) time.Duration { return 0 }

var (
	notify    = &watch.Event{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Notify{Notify: &aiserverv1.WatchGrokBotUserComputerNotify{}}}
	heartbeat = &watch.Event{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Heartbeat{Heartbeat: &aiserverv1.WatchGrokBotUserComputerHeartbeat{}}}
	pending   = &watch.Event{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Heartbeat{
		Heartbeat: &aiserverv1.WatchGrokBotUserComputerHeartbeat{PendingRequestCount: 1},
	}}
	connected = &watch.Event{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Connected{Connected: &aiserverv1.WatchGrokBotUserComputerConnected{}}}
)

// Events of a replaced stream are ignored; notify, connected and a heartbeat
// with pending requests ask for a poll; connecting shortens the silence
// timeout; an ended stream is redialed after the backoff.
func TestEvents(t *testing.T) {
	now := time.Unix(1000, 0)
	l := watch.New(noJitter)
	old := l.Start(now, "always")
	gen := l.Start(now, "always")
	assert.Equal(t, watch.Outcome{Stale: true}, l.Event(now, old, notify), "stale event used")
	assert.Equal(t, watch.Outcome{}, l.Event(now, gen, heartbeat), "empty heartbeat caused poll")
	assert.Equal(t, watch.Outcome{Work: true}, l.Event(now, gen, notify))
	assert.Equal(t, 35*time.Second, l.Timeout())
	assert.Equal(t, watch.Outcome{Work: true, Connected: true}, l.Event(now, gen, connected))
	assert.Equal(t, 15*time.Second, l.Timeout())
	assert.Equal(t, watch.Outcome{Work: true}, l.Event(now, gen, pending), "nonempty heartbeat ignored")
	assert.Equal(t, watch.Outcome{Ended: true}, l.Event(now, gen, nil))
	assert.False(t, l.Alive())
	assert.Equal(t, now.Add(time.Second), l.NextDial())
}

// Backoff doubles per failed stream up to a minute and resets on any event.
func TestBackoff(t *testing.T) {
	now := time.Unix(1000, 0)
	l := watch.New(noJitter)
	for _, seconds := range []int{1, 2, 4, 8, 16, 32, 60, 60} {
		gen := l.Start(now, "")
		l.Event(now, gen, nil)
		assert.Equal(t, now.Add(time.Duration(seconds)*time.Second), l.NextDial(), seconds)
	}
	now = now.Add(time.Hour)
	gen := l.Start(now, "")
	l.Event(now, gen, heartbeat)
	l.Event(now, gen, nil)
	assert.Equal(t, now.Add(time.Second), l.NextDial(), "backoff not reset by an event")
}

func TestTick(t *testing.T) {
	now := time.Unix(1000, 0)
	l := watch.New(noJitter)
	assert.Equal(t, watch.Dial, l.Tick(now, "always"))
	gen := l.Start(now, "always")
	assert.Equal(t, watch.None, l.Tick(now.Add(35*time.Second), "always"), "connect timeout fired early")
	assert.Equal(t, watch.Redial, l.Tick(now, "ask"), "standing change not announced")
	now = now.Add(36 * time.Second)
	assert.Equal(t, watch.Drop, l.Tick(now, "always"))
	assert.NotEqual(t, gen, l.Generation())
	assert.False(t, l.Alive())
	assert.Equal(t, now.Add(2*time.Second), l.NextDial())
	assert.Equal(t, watch.None, l.Tick(now.Add(time.Second), "always"))
	assert.Equal(t, watch.Dial, l.Tick(now.Add(2*time.Second), "always"))
	// Once connected, 15 s of silence drop the stream.
	gen = l.Start(now, "always")
	l.Event(now, gen, connected)
	assert.Equal(t, watch.None, l.Tick(now.Add(15*time.Second), "always"))
	assert.Equal(t, watch.Drop, l.Tick(now.Add(16*time.Second), "always"))
}

func TestDelayDialKeepsLatest(t *testing.T) {
	now := time.Unix(1000, 0)
	l := watch.New(noJitter)
	l.DelayDial(now, 10*time.Second)
	l.DelayDial(now, time.Second)
	require.Equal(t, now.Add(10*time.Second), l.NextDial())
}
