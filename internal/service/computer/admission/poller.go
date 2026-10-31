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
	next    time.Time
	backoff time.Duration
}

// Request asks for a poll as soon as possible.
func (p *Poller) Request() { p.need = true }

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
// backoff; more is a successful poll that returned requests, whose acks the
// next poll delivers right away.
func (p *Poller) Done(now time.Time, err error, more bool) {
	p.polling = false
	if err != nil {
		p.backoff = max(p.backoff, minPollBackoff)
		p.need = true
		p.next = now.Add(p.backoff)
		p.backoff = min(maxPollBackoff, p.backoff*2)
		return
	}
	p.backoff = minPollBackoff
	if more {
		p.need = true
	}
}
