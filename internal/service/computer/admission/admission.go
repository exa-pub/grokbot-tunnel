// Package admission decides which polled requests run, like the desktop
// provider's poll loop and supervisor: queue IDs are acknowledged and
// deduplicated, at most MaxActive requests run with MaxBacklog more waiting,
// and canceled requests never start. Scheduler and Poller are synchronous
// state machines; the caller runs the requests and the poll RPCs.
package admission

import (
	"context"
	"errors"
	"slices"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
)

const (
	// MaxActive requests execute concurrently.
	MaxActive = 8
	// MaxBacklog requests wait for a free slot.
	MaxBacklog = 64
	// PollLimit requests are fetched per poll.
	PollLimit = 64
)

// Reasons given to the agent for requests that do not run, in the desktop's
// words.
//
//nolint:revive,staticcheck // user-facing sentences
var (
	ErrBusy    = errors.New("The user's computer is busy with other requests right now; try again shortly.")
	ErrStopped = errors.New("The user's computer stopped serving requests before this one started. Retry it once the computer is back.")
)

// Request is a request frame.
type Request = aiserverv1.GrokBotUserComputerRequestFrame

// Task is an accepted request.
type Task struct {
	ID    string
	Frame *Request
	// Server is the credential's server-authoritative mode when the request
	// arrived; the request's approval flags are only meaningful under it.
	Server bool
	// Cancel stops a started task; the caller sets it when it starts the task.
	Cancel  context.CancelFunc
	started bool
}

// Verdict says what to do with a polled request frame.
type Verdict int

// Verdicts of Accept.
const (
	Ignore Verdict = iota // nothing to do
	Queued                // a task was added; call Ready
	Cancel                // the frame cancels its request; call Cancel
	Reject                // answer the frame with the returned reason
)

// Scheduler holds the accepted requests.
type Scheduler struct {
	tasks    map[string]*Task
	backlog  []string // IDs of tasks waiting to start
	active   int
	seen     dedup    // queue IDs
	requests dedup    // request IDs ever accepted; redeliveries are ignored
	canceled dedup    // request IDs canceled by the server; their results are dropped
	acks     []string // queue IDs to acknowledge on the next poll
	// early holds the request IDs that the current poll canceled before any
	// frame of theirs arrived; their work frames in the same poll are answered.
	early map[string]bool
}

// New returns an empty Scheduler.
func New() *Scheduler {
	return &Scheduler{tasks: map[string]*Task{}, seen: newDedup(), requests: newDedup(), canceled: newDedup()}
}

// Polled records a successful poll: its queue IDs are acknowledged by the
// next one. It returns the request frames not seen before.
func (s *Scheduler) Polled(queued []*aiserverv1.GrokBotUserComputerQueuedRequest) []*Request {
	s.acks = nil
	s.early = nil
	var fresh []*Request
	for _, q := range queued {
		s.acks = append(s.acks, q.Id)
		if s.seen.add(q.Id) && q.Frame != nil {
			fresh = append(fresh, q.Frame)
		}
	}
	return fresh
}

// Acks returns the queue IDs to acknowledge.
func (s *Scheduler) Acks() []string { return s.acks }

// Accept decides about a request frame polled under server mode server.
// Unrecognized frames are answered before admission, like the desktop's poll
// loop, which leaves a frame without a request ID unanswered. A work frame
// that follows the cancel of its request in the same poll is answered with
// frames.ErrCanceledBeforeStart, like the desktop's poll loop; later
// deliveries are ignored.
func (s *Scheduler) Accept(f *Request, server bool) (Verdict, error) {
	switch {
	case f.GetCancel() != nil:
		return Cancel, nil
	case f.GetRetireApproval() != nil:
		return Ignore, nil
	case s.early[f.RequestId]:
		delete(s.early, f.RequestId)
		if frames.Unrecognized(f) {
			return Reject, frames.ErrUnknownFrame
		}
		return Reject, frames.ErrCanceledBeforeStart
	case s.tasks[f.RequestId] != nil || s.requests.has(f.RequestId):
		return Ignore, nil // already queued, running or answered
	case frames.Unrecognized(f):
		if f.RequestId == "" {
			return Ignore, nil
		}
		s.requests.add(f.RequestId)
		return Reject, frames.ErrUnknownFrame
	case s.active >= MaxActive && len(s.backlog) >= MaxBacklog:
		s.requests.add(f.RequestId)
		return Reject, ErrBusy
	}
	s.requests.add(f.RequestId)
	s.tasks[f.RequestId] = &Task{ID: f.RequestId, Frame: f, Server: server}
	s.backlog = append(s.backlog, f.RequestId)
	return Queued, nil
}

// Cancel marks request id canceled, so that it never starts and its results
// are dropped, and removes it from the backlog. It returns the request's task,
// if any: a started task must be stopped through its Cancel, an unstarted one
// is forgotten and must be answered.
func (s *Scheduler) Cancel(id string) *Task {
	if !s.requests.has(id) {
		if s.early == nil {
			s.early = map[string]bool{}
		}
		s.early[id] = true
	}
	s.canceled.add(id)
	s.requests.add(id)
	s.backlog = slices.DeleteFunc(s.backlog, func(queued string) bool { return queued == id })
	t := s.tasks[id]
	if t != nil && !t.started {
		delete(s.tasks, id)
	}
	return t
}

// Canceled reports whether the server canceled request id.
func (s *Scheduler) Canceled(id string) bool { return s.canceled.has(id) }

// Ready takes tasks off the backlog while slots are free. A task runs only
// under the server mode it arrived with and with a credential (usable): the
// permission mode it was made under may be gone, and the desktop, which
// recreates its provider then, does not run it either. Tasks to run are
// counted as active until Finish; stopped tasks are forgotten and must be
// answered with ErrStopped.
func (s *Scheduler) Ready(usable, server bool) (run, stopped []*Task) {
	for s.active < MaxActive && len(s.backlog) > 0 {
		id := s.backlog[0]
		s.backlog = s.backlog[1:]
		t := s.tasks[id]
		if t == nil {
			continue // canceled while waiting
		}
		if !usable || t.Server != server {
			delete(s.tasks, id)
			stopped = append(stopped, t)
			continue
		}
		t.started = true
		s.active++
		run = append(run, t)
	}
	return run, stopped
}

// Finish forgets a task that Ready returned to run, once it is done.
func (s *Scheduler) Finish(id string) {
	delete(s.tasks, id)
	s.active--
}

// Active returns the number of running tasks.
func (s *Scheduler) Active() int { return s.active }

// Tasks maps the IDs of the tasks held to whether they started.
func (s *Scheduler) Tasks() map[string]bool {
	ids := make(map[string]bool, len(s.tasks))
	for id, t := range s.tasks {
		ids[id] = t.started
	}
	return ids
}

// Backlog returns the number of IDs waiting in the backlog, including those
// whose tasks were dropped by Drop.
func (s *Scheduler) Backlog() int { return len(s.backlog) }

// Drop forgets the tasks that have not started and returns them to be
// answered; it is used on shutdown. Their IDs stay in the backlog, where
// Ready skips them.
func (s *Scheduler) Drop() []*Task {
	var dropped []*Task
	for id, t := range s.tasks {
		if !t.started {
			dropped = append(dropped, t)
			delete(s.tasks, id)
		}
	}
	return dropped
}
