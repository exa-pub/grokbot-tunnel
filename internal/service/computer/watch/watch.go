// Package watch tracks the watch stream of the user-computer channel, like
// the desktop provider's watch loop: each stream has a generation, events of
// replaced streams are ignored, a silent stream is dropped, and failed
// streams are redialed with backoff. Loop is a synchronous state machine; the
// caller dials the streams.
package watch

import (
	"time"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

const (
	// A stream without events for this long is reconnected; before it
	// connected, the longer connectTimeout applies.
	stale          = 15 * time.Second
	connectTimeout = 35 * time.Second
	staleRedial    = 2 * time.Second
	minBackoff     = time.Second
	maxBackoff     = 60 * time.Second
)

// Event is a watch stream event.
type Event = aiserverv1.WatchGrokBotUserComputerRequestsEvent

// Loop holds the state of the current watch stream.
type Loop struct {
	jitter     func(time.Duration) time.Duration
	generation int
	alive      bool
	connected  bool
	lastEvent  time.Time
	standing   string // announced in the stream's hello
	nextDial   time.Time
	backoff    time.Duration
}

// New returns a Loop without a stream; jitter returns the random part of a
// redial delay d.
func New(jitter func(d time.Duration) time.Duration) *Loop {
	return &Loop{jitter: jitter, backoff: minBackoff}
}

// Start records a new stream announcing standing and returns its generation.
func (l *Loop) Start(now time.Time, standing string) int {
	l.Stop()
	l.alive, l.connected = true, false
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
	Ended     bool // the stream ended with the event's error; check the credential
}

// Event applies an event of the stream of generation gen: ev, or the error
// that ended the stream if ev is nil. An ended stream is redialed after the
// backoff.
func (l *Loop) Event(now time.Time, gen int, ev *Event) Outcome {
	if gen != l.generation {
		return Outcome{Stale: true}
	}
	l.lastEvent = now
	if ev != nil {
		l.backoff = minBackoff
		out := Outcome{Work: hasWork(ev), Connected: ev.GetConnected() != nil}
		if out.Connected {
			l.connected = true
		}
		return out
	}
	l.alive = false
	l.DelayDial(now, l.backoff+l.jitter(l.backoff))
	l.increaseBackoff()
	return Outcome{Ended: true}
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
	Drop          // the stream went silent; cancel it, Tick dials later
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
		l.nextDial = now.Add(staleRedial + l.jitter(l.backoff))
		l.increaseBackoff()
		return Drop
	}
	return None
}

// DelayDial postpones the next dial to at least now+d.
func (l *Loop) DelayDial(now time.Time, d time.Duration) {
	if at := now.Add(d); at.After(l.nextDial) {
		l.nextDial = at
	}
}

func (l *Loop) increaseBackoff() {
	l.backoff = min(maxBackoff, l.backoff*2)
}
