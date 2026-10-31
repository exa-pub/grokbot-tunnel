// Package replies queues the response frames of the user-computer channel
// until the server accepts them, like the desktop provider's reply queue and
// submit loop: per-request lanes, fair batches, one submit in flight with a
// short retry, and keep-alives for requests still being handled. Queue is a
// synchronous state machine; the caller runs the submit RPCs.
package replies

import (
	"errors"
	"fmt"
	"slices"
	"time"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
)

const (
	// SubmitBatch pending frames trigger an immediate submit.
	SubmitBatch = 32
	// MaxPending frames other than keep-alives that stay unsent after a full
	// round of failed submits end the session.
	MaxPending = 256
	// SubmitAttempts is a round of submits without progress.
	SubmitAttempts = 3
	retryStep      = 200 * time.Millisecond
)

// ErrBacklogFull ends the session when the server stops accepting frames.
var ErrBacklogFull = errors.New("response backlog full")

// Frame is a response frame.
type Frame = aiserverv1.GrokBotUserComputerResponseFrame

// Queue holds the frames not yet accepted by the server.
type Queue struct {
	pending []*Frame
	// previousFile alternates file chunks with metadata between batches.
	previousFile bool
	submitting   bool
	failures     int // consecutive submits without progress
	nextSubmit   time.Time
}

// Add queues f.
func (q *Queue) Add(f *Frame) { q.pending = append(q.pending, f) }

// Sink is Add as a frames.Sink, for answers built on the caller's goroutine.
func (q *Queue) Sink(f *Frame) error {
	q.Add(f)
	return nil
}

// Take queues a reply of a worker. The reply of a canceled request is
// dropped unless it ends the request's stream, and the last chunk of a
// canceled transfer becomes an error. It reports whether a full batch is
// pending.
func (q *Queue) Take(f *Frame, canceled bool) bool {
	if canceled {
		if f.GetControl() == nil && !frames.Final(f) {
			return false
		}
		f = frames.CanceledFile(f)
	}
	q.Add(f)
	return len(q.pending) >= SubmitBatch
}

// Cancel discards the undelivered frames of request id, keeping those that
// end its stream.
func (q *Queue) Cancel(id string) {
	q.pending = slices.DeleteFunc(q.pending, func(f *Frame) bool { return f.RequestId == id && !frames.Keep(f) })
	for i, f := range q.pending {
		if f.RequestId == id {
			q.pending[i] = frames.CanceledFile(f)
		}
	}
}

// Pending returns the frames not yet accepted, in order.
func (q *Queue) Pending() []*Frame { return q.pending }

// Stuck counts the pending frames other than keep-alives, which are bounded
// by the number of requests and must not end the session on their own.
func (q *Queue) Stuck() int {
	n := 0
	for _, f := range q.pending {
		if f.GetMessagesAccepted() == nil {
			n++
		}
	}
	return n
}

// Accepting reports whether workers may hand over more replies. They wait
// while enough frames are pending, as a blocking submit used to.
func (q *Queue) Accepting() bool {
	return q.Stuck() < MaxPending && (!q.submitting || len(q.pending) < SubmitBatch)
}

// Submitting reports whether a submit is in flight.
func (q *Queue) Submitting() bool { return q.submitting }

// Next returns the batch to submit now and marks a submit in flight, or nil
// while one is in flight, backing off or nothing is pending.
func (q *Queue) Next(now time.Time) []*Frame {
	if q.submitting || len(q.pending) == 0 || now.Before(q.nextSubmit) {
		return nil
	}
	q.submitting = true
	return q.Batch()
}

// Accepted removes the first n frames of a submitted batch from the queue.
func (q *Queue) Accepted(batch []*Frame, n int) {
	done := map[*Frame]bool{}
	for _, f := range batch[:n] {
		done[f] = true
	}
	q.pending = slices.DeleteFunc(q.pending, func(f *Frame) bool { return done[f] })
}

// Settle ends the submit in flight with n of batch accepted, without retry
// bookkeeping.
func (q *Queue) Settle(batch []*Frame, n int) {
	q.submitting = false
	q.Accepted(batch, n)
}

// Submitted applies the result of the submit in flight. again is true after
// progress: the caller submits the rest right away. Otherwise the next submit
// waits a growing delay, if retry allows; after SubmitAttempts failures, or
// without retry, exhausted is true and the round starts over on the caller's
// next tick.
func (q *Queue) Submitted(now time.Time, batch []*Frame, n int, retry bool) (again, exhausted bool) {
	q.Settle(batch, n)
	if n > 0 {
		q.failures = 0
		q.nextSubmit = time.Time{}
		return true, false
	}
	q.failures++
	if q.failures < SubmitAttempts && retry {
		q.nextSubmit = now.Add(time.Duration(q.failures) * retryStep)
		return false, false
	}
	q.failures = 0
	q.nextSubmit = time.Time{}
	return false, true
}

// Overflow returns ErrBacklogFull wrapping err once a failed round leaves too
// many frames stuck.
func (q *Queue) Overflow(err error) error {
	if q.Stuck() < MaxPending {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrBacklogFull, err)
}
