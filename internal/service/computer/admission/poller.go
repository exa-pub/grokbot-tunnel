package admission

import "time"

// Poll backoff after failures.
const (
	minPollBackoff = time.Second
	maxPollBackoff = 60 * time.Second
)

// Poller schedules polls: one in flight, on request, and with backoff after a
// failure instead of waiting for the next poll interval.
type Poller struct {
	need    bool
	polling bool
	// triggered is set by Trigger during the poll in flight, whose failure
	// then does not delay the next poll.
	triggered bool
	next      time.Time
	backoff   time.Duration
}

// Request asks for a poll as soon as possible.
func (p *Poller) Request() { p.need = true }

// Trigger asks for a poll now, without waiting for the backoff of failed
// polls; during a poll, it asks for another right after it, even if it
// fails. It is used when the server says requests are waiting (notify, a
// heartbeat with pending requests), like the desktop's requestPoll, which
// has no poll backoff at all. The backoff keeps growing and paces only the
// retries that no event asked for, so while polls keep failing they follow
// the server's events, one in flight, and not faster.
func (p *Poller) Trigger() {
	p.need = true
	p.next = time.Time{}
	if p.polling {
		p.triggered = true
	}
}

// Reset forgets the backoff of failed polls and asks for a poll now. It is
// used when the channel comes back, like the desktop's connected handler,
// which polls at once and has no poll backoff at all.
func (p *Poller) Reset() {
	p.need = true
	p.next = time.Time{}
	p.backoff = minPollBackoff
}

// Needed reports whether a poll was requested and not yet started.
func (p *Poller) Needed() bool { return p.need }

// Polling reports whether a poll is in flight.
func (p *Poller) Polling() bool { return p.polling }

// Start reports whether a poll should start now and marks it in flight.
func (p *Poller) Start(now time.Time) bool {
	if p.polling || !p.need || now.Before(p.next) {
		return false
	}
	p.need, p.polling = false, true
	return true
}

// Done applies the result of the poll in flight: a failure is retried with
// backoff, unless Trigger asked for a poll meanwhile; more is a successful
// poll that returned requests, whose acks the next poll delivers right away.
func (p *Poller) Done(now time.Time, err error, more bool) {
	p.polling = false
	triggered := p.triggered
	p.triggered = false
	if err != nil {
		p.backoff = max(p.backoff, minPollBackoff)
		p.need = true
		if !triggered {
			p.next = now.Add(p.backoff)
		}
		p.backoff = min(maxPollBackoff, p.backoff*2)
		return
	}
	p.backoff = minPollBackoff
	if more {
		p.need = true
	}
}
