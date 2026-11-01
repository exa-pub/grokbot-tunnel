// Package watch tracks the watch stream of the user-computer channel, like
// the desktop provider's watch loop (runLoop): each stream has a generation,
// events of replaced streams are ignored, a silent stream is dropped, and
// ended streams are redialed after the delay their outcome calls for. Loop is
// a synchronous state machine; the caller dials the streams.
package watch

import (
	"time"

	"connectrpc.com/connect"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

const (
	// A stream without events for this long is reconnected; before it
	// connected, the longer connectTimeout applies.
	stale          = 15 * time.Second
	connectTimeout = 35 * time.Second
	// A failed or stalled stream is redialed after 1 s, doubling up to a
	// minute without jitter, like the desktop's user-computer-watch-reconnect;
	// the count restarts once a stream connects.
	minReconnect = time.Second
	maxReconnect = 60 * time.Second
	// A stream that went silent after it connected is redialed after a random
	// delay of up to 2 s, doubling up to a minute, like the desktop's
	// user-computer-watch-silent-renewal (full jitter).
	minSilentRenewal = 2 * time.Second
	maxSilentRenewal = 60 * time.Second
)

// Event is a watch stream event.
type Event = aiserverv1.WatchGrokBotUserComputerRequestsEvent

// Loop holds the state of the current watch stream.
type Loop struct {
	random     func(time.Duration) time.Duration
	generation int
	alive      bool
	connected  bool
	heard      bool // an event arrived after the stream connected
	lastEvent  time.Time
	standing   string // announced in the stream's hello
	nextDial   time.Time
	reconnects int // failed or stalled streams since one connected
	silences   int // streams in a row that went silent without being heard after connecting
}

// New returns a Loop without a stream; random returns a random delay of at
// most d.
func New(random func(d time.Duration) time.Duration) *Loop {
	return &Loop{random: random}
}

// Start records a new stream announcing standing and returns its generation.
func (l *Loop) Start(now time.Time, standing string) int {
	l.Stop()
	l.alive, l.connected, l.heard = true, false, false
	l.lastEvent, l.standing = now, standing
	return l.generation
}

// Stop forgets the current stream; its later events are ignored.
func (l *Loop) Stop() {
	l.generation++
	l.alive = false
}

// Generation returns the generation of the current stream.
func (l *Loop) Generation() int { return l.generation }

// Alive reports whether a stream is open.
func (l *Loop) Alive() bool { return l.alive }

// NextDial returns the earliest time of the next dial.
func (l *Loop) NextDial() time.Time { return l.nextDial }

// Timeout returns how long the stream may stay silent.
func (l *Loop) Timeout() time.Duration {
	if l.connected {
		return stale
	}
	return connectTimeout
}

// Outcome is what an event means for the caller.
type Outcome struct {
	Stale     bool // from a replaced stream; ignore it
	Work      bool // requests may be waiting; poll
	Connected bool // the server accepted the credential
	Ended     bool // the stream ended with an error; check the credential
}

// Event applies an event of the stream of generation gen.
func (l *Loop) Event(now time.Time, gen int, ev *Event) Outcome {
	if gen != l.generation {
		return Outcome{Stale: true}
	}
	l.lastEvent = now
	if l.connected {
		l.heard = true
	}
	out := Outcome{Work: hasWork(ev), Connected: ev.GetConnected() != nil}
	if out.Connected {
		l.connected = true
		l.reconnects = 0
	}
	return out
}

// End applies the end of the stream of generation gen with err, nil if the
// server closed it. Like the desktop, a connected stream the server closed
// (its lifetime) is redialed at once; a rejected credential leaves the delay
// to the caller; other failures are redialed after the reconnect backoff.
// Go+: a stream closed before it connected counts as failed, so that a
// server closing streams at once is not redialed in a loop.
func (l *Loop) End(now time.Time, gen int, err error) Outcome {
	if gen != l.generation {
		return Outcome{Stale: true}
	}
	l.lastEvent = now
	l.alive = false
	l.silences = 0
	switch {
	case err == nil && l.connected:
	case connect.CodeOf(err) == connect.CodeUnauthenticated:
		l.reconnects = 0
	default:
		l.reconnect(now)
	}
	return Outcome{Ended: err != nil}
}

// hasWork reports whether a watch event says requests may be waiting.
func hasWork(ev *Event) bool {
	return ev.GetConnected() != nil || ev.GetNotify() != nil || ev.GetHeartbeat().GetPendingRequestCount() > 0
}

// Action is what Tick asks the caller to do.
type Action int

// Actions of Tick.
const (
	None   Action = iota
	Dial          // dial a new stream, then poll
	Redial        // replace the stream to announce a new standing mode
	Drop          // the stream stalled before connecting; cancel it, Tick dials later
	Renew         // the stream went silent after connecting; cancel it and renew the connections, Tick dials later
)

// Tick decides about the stream at now, given the standing mode to announce.
func (l *Loop) Tick(now time.Time, standing string) Action {
	switch {
	case !l.alive && !now.Before(l.nextDial):
		return Dial
	case l.alive && l.standing != standing:
		return Redial
	case l.alive && now.Sub(l.lastEvent) > l.Timeout():
		l.Stop()
		if !l.connected {
			l.silences = 0
			l.reconnect(now)
			return Drop
		}
		if l.heard {
			l.silences = 1
		} else {
			l.silences++
		}
		d := min(maxSilentRenewal, minSilentRenewal<<min(l.silences-1, 5))
		l.DelayDial(now, l.random(d))
		return Renew
	}
	return None
}

// DelayDial postpones the next dial to at least now+d.
func (l *Loop) DelayDial(now time.Time, d time.Duration) {
	if at := now.Add(d); at.After(l.nextDial) {
		l.nextDial = at
	}
}

// reconnect delays the next dial by the reconnect backoff.
func (l *Loop) reconnect(now time.Time) {
	l.DelayDial(now, min(maxReconnect, minReconnect<<min(l.reconnects, 6)))
	l.reconnects++
}
