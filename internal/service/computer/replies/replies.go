// Package replies queues the response frames of the user-computer channel
// until the server accepts them, like the desktop provider's reply queue and
// submit loop: per-request lanes, fair batches, one submit in flight with a
// short retry, and keep-alives for requests still being handled. Queue is a
// synchronous state machine; the caller runs the submit RPCs.
package replies

import (
	"slices"
	"time"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
)

const (
	// SubmitBatch pending frames trigger an immediate submit.
	SubmitBatch = 32
	// MaxPending frames other than keep-alives make workers wait.
	MaxPending = 256
	// MaxPendingData bytes of unsent file data make workers wait too. The
	// desktop reads at most one chunk ahead per download (MX.readAhead);
	// this leaves room for one chunk of each of 8 concurrent downloads.
	MaxPendingData = 8 << 20
	// SubmitAttempts is a round of submits without progress, retried after
	// a constant retryStep like the desktop's user-computer-submit.
	SubmitAttempts = 3
	retryStep      = 200 * time.Millisecond
)

// Frame is a response frame.
type Frame = aiserverv1.GrokBotUserComputerResponseFrame

// Queue holds the frames not yet accepted by the server.
type Queue struct {
	pending []*Frame
	// order is the order in which Batch serves the requests' lanes: like the
	// desktop's MX, a new lane goes to the end, and so does a lane whose
	// frames were accepted. ordered holds its IDs.
	order   []string
	ordered map[string]bool
	// previousFile alternates file chunks with metadata between batches.
	previousFile bool
	submitting   bool
	inflight     map[*Frame]bool // the batch of the submit in flight
	failures     int             // consecutive submits without progress in this round
	roundStart   time.Time       // when the first submit of this round started
	nextSubmit   time.Time
	// closingSince is when the round started in which a kept closing frame
	// first failed; its budget counts from there.
	closingSince   map[*Frame]time.Time
	closingRetries int  // failed rounds with only closing frames left
	closingWait    bool // nextSubmit is the pause after such a round
	// partial holds the downloads replaced by ErrPartialFile until their last
	// frame arrives; their later chunks are dropped.
	partial map[string]bool
}

// Add queues f. Like the desktop, whose enqueue flushes at once, a frame
// other than a keep-alive ends the pause after a failed round of closing
// frames, which then go along with it.
func (q *Queue) Add(f *Frame) {
	if q.closingSince == nil {
		q.closingSince, q.partial, q.ordered = map[*Frame]time.Time{}, map[string]bool{}, map[string]bool{}
	}
	if !q.ordered[f.RequestId] {
		q.ordered[f.RequestId] = true
		q.order = append(q.order, f.RequestId)
	}
	if q.closingWait && f.GetMessagesAccepted() == nil {
		q.closingWait = false
		q.nextSubmit = time.Time{}
	}
	q.pending = append(q.pending, f)
}

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
	if q.partial[f.RequestId] && (f.GetFile() != nil || f.GetFileError() != nil) {
		if frames.Final(f) {
			delete(q.partial, f.RequestId) // nothing more comes
		}
		return false // the download already ended with ErrPartialFile
	}
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
// end its stream. Frames of the submit in flight stay as they are: the
// server may accept them, and a replaced last chunk would end the request
// twice (bug B6). A failed round drops them later as frames of a canceled
// request.
func (q *Queue) Cancel(id string) {
	drop := map[*Frame]bool{}
	for i, f := range q.pending {
		switch {
		case f.RequestId != id || q.inflight[f]:
		case !frames.Keep(f):
			drop[f] = true
		default:
			if g := frames.CanceledFile(f); g != f {
				delete(q.closingSince, f)
				q.pending[i] = g
			}
		}
	}
	q.remove(drop)
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
// while enough frames or file data are pending, as a blocking submit used
// to.
func (q *Queue) Accepting() bool {
	if q.submitting && len(q.pending) >= SubmitBatch {
		return false
	}
	stuck, data := 0, 0
	for _, f := range q.pending {
		if f.GetMessagesAccepted() == nil {
			stuck++
		}
		data += len(f.GetFile().GetData())
	}
	return stuck < MaxPending && data < MaxPendingData
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
	q.closingWait = false
	if q.failures == 0 {
		q.roundStart = now
	}
	batch := q.Batch()
	q.inflight = make(map[*Frame]bool, len(batch))
	for _, f := range batch {
		q.inflight[f] = true
	}
	return batch
}

// Accepted removes the first n frames of a submitted batch from the queue
// and moves their requests to the end of the serving order.
func (q *Queue) Accepted(batch []*Frame, n int) {
	done := map[*Frame]bool{}
	served := map[string]bool{}
	var tail []string
	for _, f := range batch[:n] {
		done[f] = true
		if !served[f.RequestId] {
			served[f.RequestId] = true
			tail = append(tail, f.RequestId)
		}
	}
	q.remove(done)
	if n == 0 {
		return
	}
	order := make([]string, 0, len(q.order)+len(tail))
	for _, id := range q.order {
		if !served[id] {
			order = append(order, id)
		}
	}
	q.order = append(order, tail...)
}

// Settle ends the submit in flight with n of batch accepted, without retry
// bookkeeping.
func (q *Queue) Settle(batch []*Frame, n int) {
	q.submitting = false
	q.inflight = nil
	q.Accepted(batch, n)
}

// Submitted applies the result of the submit in flight. again is true after
// progress: the caller submits the rest right away. A failure is retried
// after 200 ms, twice; the third ends the round, which keeps only the
// frames that close their requests (see fail) and returns a non-nil Failure.
// rejected means the server rejected the credential: nothing is dropped, and
// the frames wait for a new credential.
func (q *Queue) Submitted(now time.Time, batch []*Frame, n int, rejected bool, canceled func(string) bool) (again bool, failed *Failure) {
	q.Settle(batch, n)
	if n > 0 {
		q.failures, q.closingRetries = 0, 0
		q.nextSubmit = time.Time{}
		return true, nil
	}
	if rejected {
		q.failures = 0
		q.nextSubmit = time.Time{}
		return false, nil
	}
	q.failures++
	if q.failures < SubmitAttempts {
		q.nextSubmit = now.Add(retryStep)
		return false, nil
	}
	q.failures = 0
	q.nextSubmit = time.Time{}
	out := q.fail(now, batch, canceled)
	return out.Kept < len(q.pending), &out
}

func (q *Queue) remove(frames map[*Frame]bool) {
	if len(frames) == 0 {
		return
	}
	for f := range frames {
		delete(q.closingSince, f)
	}
	q.pending = slices.DeleteFunc(q.pending, func(f *Frame) bool { return frames[f] })
}
