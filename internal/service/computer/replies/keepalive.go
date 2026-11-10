package replies

import (
	"cmp"
	"slices"
	"time"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

// The desktop provider's keep-alive timing.
const (
	// AliveTick is how often the caller asks KeepAlive.Next.
	AliveTick = 500 * time.Millisecond
	// AliveTimeout is the deadline of a keep-alive submit.
	AliveTimeout = 3 * time.Second
	// StaleSubmit is how long a round of submits may take before the
	// requests of its batch are no longer kept alive: the deadline of one
	// submit.
	StaleSubmit = 120 * time.Second

	aliveAfterAdmission = time.Second             // the first keep-alive of a request
	aliveAfterHeard     = 3 * time.Second         // after the server accepted a frame of the request
	aliveAhead          = 1500 * time.Millisecond // requests due this soon go along with one that is due
	alivePause          = 5 * time.Second         // after the server answered that it is exhausted
	aliveBatch          = 256
)

// KeepAlive tells the server, with messages_accepted frames, that requests
// it has not heard of for a while are alive, like the desktop's
// sendKeepAlives: requests that wait for their turn and requests with
// frames the server has not accepted yet. A request that runs and has
// nothing to send is not among them; the heartbeats of its exec stream show
// that it is alive. The frames go in a submit of their own, so that a submit
// of data that takes long does not hold them back. KeepAlive is a
// synchronous state machine; the caller runs the submit.
type KeepAlive struct {
	// due is when the server should next hear of a request.
	due     map[string]time.Time
	paused  time.Time
	sending bool
}

// Admitted notes that request id was accepted, to run or to wait.
func (k *KeepAlive) Admitted(id string, now time.Time) {
	k.set(id, now.Add(aliveAfterAdmission))
}

// Heard notes that the server accepted frames.
func (k *KeepAlive) Heard(frames []*Frame, now time.Time) {
	for _, f := range frames {
		k.set(f.RequestId, now.Add(aliveAfterHeard))
	}
}

// Next returns the keep-alives to submit now, or nil, and forgets the
// requests that are not in held, those the caller still has anything of.
// Once a request in eligible is due, those due soon go along with it,
// earliest first. Nothing is returned while a keep-alive submit is in flight
// or the server asked for a pause. Pass the result of the submit to Sent.
func (k *KeepAlive) Next(now time.Time, held, eligible map[string]bool) []*Frame {
	for id := range k.due {
		if !held[id] {
			delete(k.due, id)
		}
	}
	if k.sending || now.Before(k.paused) {
		return nil
	}
	var ids []string
	overdue := false
	for id, due := range k.due {
		if !eligible[id] || due.After(now.Add(aliveAhead)) {
			continue
		}
		ids = append(ids, id)
		overdue = overdue || !due.After(now)
	}
	if !overdue {
		return nil
	}
	slices.SortFunc(ids, func(a, b string) int {
		if c := k.due[a].Compare(k.due[b]); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})
	if len(ids) > aliveBatch {
		ids = ids[:aliveBatch]
	}
	batch := make([]*Frame, len(ids))
	for i, id := range ids {
		batch[i] = &Frame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_MessagesAccepted{
			MessagesAccepted: &aiserverv1.GrokBotUserComputerMessagesAccepted{},
		}}
	}
	k.sending = true
	return batch
}

// Sent applies the result of the submit of batch: the server heard of its
// requests, or, exhausted, asks for a pause. Any other failure is tried
// again on the next tick.
func (k *KeepAlive) Sent(now time.Time, batch []*Frame, ok, exhausted bool) {
	k.sending = false
	switch {
	case ok:
		k.Heard(batch, now)
	case exhausted:
		k.paused = now.Add(alivePause)
	}
}

func (k *KeepAlive) set(id string, due time.Time) {
	if k.due == nil {
		k.due = map[string]time.Time{}
	}
	k.due[id] = due
}

// Unheard returns the requests with frames the server has not accepted:
// held are all of them, alive those to keep alive. Like the desktop's
// keepAliveEligibleIds, the requests of the submit in flight are left out of
// alive once its round has taken StaleSubmit.
func (q *Queue) Unheard(now time.Time) (held, alive map[string]bool) {
	held, alive = map[string]bool{}, map[string]bool{}
	stale := q.submitting && now.Sub(q.roundStart) >= StaleSubmit
	stuck := map[string]bool{}
	if stale {
		for f := range q.inflight {
			stuck[f.RequestId] = true
		}
	}
	for _, f := range q.pending {
		held[f.RequestId] = true
		if !stuck[f.RequestId] {
			alive[f.RequestId] = true
		}
	}
	return held, alive
}
