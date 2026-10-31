package computer

import (
	"context"
	"errors"
	"log"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/admission"
)

func (s *session) accept(f *requestFrame, server bool) {
	if s.pv.Debug {
		if raw, err := protojson.Marshal(f); err == nil {
			log.Printf("request JSON: %s", raw)
		}
	}
	switch verdict, reason := s.admit.Accept(f, server); verdict {
	case admission.Cancel:
		s.cancel(f.RequestId)
	case admission.Reject:
		if err := frames.Reject(f, reason, s.out.Sink); err != nil {
			log.Printf("reject %s: %v", f.RequestId, err)
		}
	case admission.Queued:
		s.startJobs()
	case admission.Ignore:
	}
}

// cancel stops a request and discards its undelivered results. The request
// is remembered so a late or repeated delivery does not start it again.
func (s *session) cancel(id string) {
	s.out.Cancel(id)
	switch t := s.admit.Cancel(id); {
	case t == nil:
	case t.Cancel != nil:
		t.Cancel() // the worker reports the cancellation
	default:
		closeCanceled(t.Frame, s.out.Sink)
	}
}

// closeCanceled answers a request canceled before it started.
func closeCanceled(f *requestFrame, sink frames.Sink) {
	if err := frames.Canceled(f, sink); err != nil {
		log.Printf("cancel %s: %v", f.RequestId, err)
	}
}

// startJobs starts backlogged tasks while there are free slots.
func (s *session) startJobs() {
	c := s.cred.Current()
	run, stopped := s.admit.Ready(c.Credential != "", c.ServerAuthoritative)
	for _, t := range stopped {
		if err := frames.Reject(t.Frame, admission.ErrStopped, s.out.Sink); err != nil {
			log.Printf("reject %s: %v", t.ID, err)
		}
	}
	for _, t := range run {
		ctx, cancel := context.WithCancel(s.work)
		t.Cancel = cancel
		go func() {
			defer cancel()
			if err := s.pv.Handler.Handle(ctx, t.Frame, t.Server, s.sink); err != nil {
				log.Printf("request %s: %v", t.ID, err)
			}
			select {
			case s.done <- t.ID:
			case <-s.life.Done():
			}
		}()
	}
}

// startSubmit sends the next batch in the background unless a submit is
// already in flight or backing off. The submit outlives shutdown, which waits
// for it: aborting a batch the server may have accepted would resend it.
func (s *session) startSubmit() {
	credential := s.cred.Current().Credential
	if credential == "" {
		return
	}
	batch := s.out.Next(s.pv.now())
	if batch == nil {
		return
	}
	go func() {
		n, err := s.pv.submitFrames(s.life, credential, batch)
		s.submitted <- submitted{credential: credential, frames: batch, accepted: n, err: err}
	}()
}

// onSubmitted applies a submit result and keeps draining the replies.
func (s *session) onSubmitted(r submitted) error {
	if r.err != nil {
		s.invalidate(r.credential, r.err)
	}
	again, exhausted := s.out.Submitted(s.pv.now(), r.frames, r.accepted, s.cred.Current().Credential != "")
	switch {
	case again:
		s.startSubmit()
	case exhausted:
		// A full round failed; the next tick starts another one.
		log.Printf("submit: %v", r.err)
		return s.out.Overflow(r.err)
	}
	return nil
}

// startPoll fetches queued requests in the background and acknowledges the
// previous batch. Shutdown abandons it.
func (s *session) startPoll() {
	c := s.cred.Current()
	if c.Credential == "" || !s.poll.Start(s.pv.now()) {
		return
	}
	req := &aiserverv1.PollGrokBotUserComputerRequestsRequest{
		MachineId:  s.pv.Machine.MachineID,
		Credential: c.Credential,
		AckIds:     s.admit.Acks(),
		Limit:      admission.PollLimit,
	}
	go func() {
		ctx, cancel := rpcContext(s.work)
		defer cancel()
		r, err := s.pv.Channel.PollGrokBotUserComputerRequests(ctx, connect.NewRequest(req))
		result := polled{credential: c.Credential, server: c.ServerAuthoritative, err: err}
		if err == nil {
			result.requests = r.Msg.Requests
		}
		s.polled <- result
	}()
}

func (s *session) onPolled(r polled) {
	if r.err != nil {
		s.invalidate(r.credential, r.err)
		if !errors.Is(r.err, context.Canceled) {
			log.Printf("poll: %v", r.err)
		}
		s.poll.Done(s.pv.now(), r.err, false)
		return
	}
	for _, f := range s.admit.Polled(r.requests) {
		s.accept(f, r.server)
	}
	// Poll again right away to deliver the acks and drain the queue.
	s.poll.Done(s.pv.now(), nil, len(s.admit.Acks()) > 0)
}
