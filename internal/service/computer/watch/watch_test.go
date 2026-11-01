package watch_test

import (
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/watch"
)

func noJitter(time.Duration) time.Duration { return 0 }

// longest picks the longest delay a full jitter allows.
func longest(d time.Duration) time.Duration { return d }

var (
	notify    = &watch.Event{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Notify{Notify: &aiserverv1.WatchGrokBotUserComputerNotify{}}}
	heartbeat = &watch.Event{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Heartbeat{Heartbeat: &aiserverv1.WatchGrokBotUserComputerHeartbeat{}}}
	pending   = &watch.Event{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Heartbeat{
		Heartbeat: &aiserverv1.WatchGrokBotUserComputerHeartbeat{PendingRequestCount: 1},
	}}
	connected = &watch.Event{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Connected{Connected: &aiserverv1.WatchGrokBotUserComputerConnected{}}}

	down     = connect.NewError(connect.CodeUnavailable, errors.New("down"))
	rejected = connect.NewError(connect.CodeUnauthenticated, errors.New("rejected"))
)

// Events of a replaced stream are ignored; notify, connected and a heartbeat
// with pending requests ask for a poll; connecting shortens the silence
// timeout; a failed stream is redialed after the backoff.
func TestEvents(t *testing.T) {
	now := time.Unix(1000, 0)
	l := watch.New(noJitter)
	old := l.Start(now, "always")
	gen := l.Start(now, "always")
	assert.Equal(t, watch.Outcome{Stale: true}, l.Event(now, old, notify), "stale event used")
	assert.Equal(t, watch.Outcome{Stale: true}, l.End(now, old, down), "stale end used")
	assert.Equal(t, watch.Outcome{}, l.Event(now, gen, heartbeat), "empty heartbeat caused poll")
	assert.Equal(t, watch.Outcome{Work: true}, l.Event(now, gen, notify))
	assert.Equal(t, 35*time.Second, l.Timeout())
	assert.Equal(t, watch.Outcome{Work: true, Connected: true}, l.Event(now, gen, connected))
	assert.Equal(t, 15*time.Second, l.Timeout())
	assert.Equal(t, watch.Outcome{Work: true}, l.Event(now, gen, pending), "nonempty heartbeat ignored")
	assert.Equal(t, watch.Outcome{Ended: true}, l.End(now, gen, down))
	assert.False(t, l.Alive())
	assert.Equal(t, now.Add(time.Second), l.NextDial())
}

// step is one stream of a backoff table: how it goes and the delay of the
// next dial that follows.
type step struct {
	connect bool          // the stream reports connected first
	heard   bool          // an event follows connected
	end     error         // the error the stream ends with
	clean   bool          // the server closes the stream instead
	silent  bool          // the stream times out instead
	action  watch.Action  // Tick's action for a silent stream
	delay   time.Duration // until the next dial
}

// Like the desktop's runLoop: a failed or stalled stream waits 1 s doubling
// up to 60 s, without jitter, reset once a stream connects; a connected
// stream the server closes is redialed at once and leaves the backoff alone;
// a stream that goes silent after connecting is renewed after a full jitter
// of 2 s doubling up to 60 s, restarting once a silent stream was heard
// after connecting; a rejected credential resets the backoff and leaves the
// delay to the caller.
func TestBackoff(t *testing.T) {
	failed := step{end: down}
	clean := step{connect: true, clean: true}
	stall := step{silent: true, action: watch.Drop}
	silent := step{connect: true, silent: true, action: watch.Renew}
	heardSilent := step{connect: true, heard: true, silent: true, action: watch.Renew}
	with := func(s step, d time.Duration) step { s.delay = d; return s }
	for _, tc := range []struct {
		name  string
		steps []step
	}{
		{"failures double up to a minute", []step{
			with(failed, time.Second), with(failed, 2*time.Second), with(failed, 4*time.Second), with(stall, 8*time.Second),
			with(failed, 16*time.Second), with(failed, 32*time.Second), with(failed, 60*time.Second), with(stall, 60*time.Second),
		}},
		{"clean end of a connected stream", []step{
			with(failed, time.Second), with(failed, 2*time.Second), with(clean, 0), with(clean, 0), with(failed, time.Second),
		}},
		{"clean end before connecting counts as failed", []step{
			with(step{clean: true}, time.Second), with(step{clean: true}, 2*time.Second),
		}},
		{"connected resets the failures", []step{
			with(failed, time.Second), with(failed, 2*time.Second), with(step{connect: true, end: down}, time.Second),
		}},
		{"silent streams", []step{
			with(silent, 2*time.Second), with(silent, 4*time.Second), with(silent, 8*time.Second), with(heardSilent, 2*time.Second),
			with(silent, 4*time.Second), with(failed, time.Second), with(silent, 2*time.Second),
		}},
		{"silent streams up to a minute", []step{
			with(silent, 2*time.Second), with(silent, 4*time.Second), with(silent, 8*time.Second), with(silent, 16*time.Second),
			with(silent, 32*time.Second), with(silent, 60*time.Second), with(silent, 60*time.Second),
		}},
		{"rejected credential", []step{
			with(failed, time.Second), with(failed, 2*time.Second), with(step{end: rejected}, 0), with(failed, time.Second),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			l := watch.New(longest)
			for i, s := range tc.steps {
				require.Equal(t, watch.Dial, l.Tick(now, ""), i)
				gen := l.Start(now, "")
				if s.connect {
					l.Event(now, gen, connected)
				}
				if s.heard {
					l.Event(now, gen, heartbeat)
				}
				switch {
				case s.silent:
					now = now.Add(time.Minute)
					require.Equal(t, s.action, l.Tick(now, ""), i)
				case s.clean:
					assert.Equal(t, watch.Outcome{}, l.End(now, gen, nil), i)
				default:
					assert.Equal(t, watch.Outcome{Ended: true}, l.End(now, gen, s.end), i)
				}
				assert.False(t, l.Alive(), i)
				if s.delay == 0 {
					assert.False(t, l.NextDial().After(now), "step %d: dial delayed", i)
				} else {
					assert.Equal(t, now.Add(s.delay), l.NextDial(), i)
				}
				now = now.Add(s.delay)
			}
		})
	}
}

// The silent renewal delay is a full jitter: anywhere from 0 to its cap.
func TestSilentRenewalJitter(t *testing.T) {
	now := time.Unix(1000, 0)
	var capped []time.Duration
	l := watch.New(func(d time.Duration) time.Duration { capped = append(capped, d); return d / 4 })
	gen := l.Start(now, "")
	l.Event(now, gen, connected)
	now = now.Add(time.Minute)
	require.Equal(t, watch.Renew, l.Tick(now, ""))
	assert.Equal(t, []time.Duration{2 * time.Second}, capped)
	assert.Equal(t, now.Add(500*time.Millisecond), l.NextDial())
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
	assert.Equal(t, now.Add(time.Second), l.NextDial())
	assert.Equal(t, watch.None, l.Tick(now.Add(time.Second-time.Millisecond), "always"))
	assert.Equal(t, watch.Dial, l.Tick(now.Add(time.Second), "always"))
	// Once connected, 15 s of silence renew the stream.
	gen = l.Start(now, "always")
	l.Event(now, gen, connected)
	assert.Equal(t, watch.None, l.Tick(now.Add(15*time.Second), "always"))
	assert.Equal(t, watch.Renew, l.Tick(now.Add(16*time.Second), "always"))
}

func TestDelayDialKeepsLatest(t *testing.T) {
	now := time.Unix(1000, 0)
	l := watch.New(noJitter)
	l.DelayDial(now, 10*time.Second)
	l.DelayDial(now, time.Second)
	require.Equal(t, now.Add(10*time.Second), l.NextDial())
}
