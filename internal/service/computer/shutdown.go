package computer

import (
	"context"
	"log"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/admission"

	"connectrpc.com/connect"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/replies"
)

const (
	shutdownGrace = 8 * time.Second
	// Part of shutdownGrace kept for the final submit after waiting for workers.
	finalFlushGrace = 2 * time.Second
	// The presence is withdrawn on shutdown within this time, as the desktop's
	// user-computer-presence-withdraw deadline.
	withdrawTimeout = 750 * time.Millisecond
	// submitRetryStep grows the pause between failed final submits.
	submitRetryStep = 200 * time.Millisecond
)

// withdrawPresence tells the server in the background that the session with
// presence is gone, so the computer goes offline at once instead of when the
// stream times out. The channel is closed once the call is done.
func (pv *Provider) withdrawPresence(credential, presence string) <-chan struct{} {
	done := make(chan struct{})
	if credential == "" {
		close(done)
		return done
	}
	req := &aiserverv1.WithdrawGrokBotUserComputerPresenceRequest{
		MachineId:     pv.Machine.MachineID,
		Credential:    credential,
		PresenceToken: presence,
	}
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), withdrawTimeout)
		defer cancel()
		if _, err := pv.Channel.WithdrawGrokBotUserComputerPresence(ctx, connect.NewRequest(req)); err != nil {
			log.Printf("withdraw presence: %v", err)
		}
	}()
	return done
}

// ackOnShutdown acknowledges the last polled requests in the background, so
// that the server does not deliver them again after a restart and they do
// not run twice (bug B5). There is no ack RPC, so it polls with the acks;
// requests that poll returns are neither started nor acknowledged and come
// back later. Go+: the desktop does not poll while closing. The channel is
// closed once the call is done.
func (pv *Provider) ackOnShutdown(credential string, acks []string) <-chan struct{} {
	done := make(chan struct{})
	if credential == "" || len(acks) == 0 {
		close(done)
		return done
	}
	req := &aiserverv1.PollGrokBotUserComputerRequestsRequest{
		MachineId:  pv.Machine.MachineID,
		Credential: credential,
		AckIds:     acks,
		Limit:      1, // 0 could mean the server's default
	}
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), withdrawTimeout)
		defer cancel()
		if _, err := pv.Channel.PollGrokBotUserComputerRequests(ctx, connect.NewRequest(req)); err != nil {
			log.Printf("acknowledge requests: %v", err)
		}
	}()
	return done
}

// shutdown cancels all work and flushes results within shutdownGrace, keeping
// finalFlushGrace of it for the final submit even if workers are slow.
func (s *session) shutdown() {
	s.stopWork()
	if s.watchCancel != nil {
		s.watchCancel()
	}
	// Like the desktop, requests that never started learn that the computer
	// stopped serving.
	for _, t := range s.admit.Drop() {
		if err := frames.Reject(t.Frame, admission.ErrStopped, s.out.Sink); err != nil {
			log.Printf("reject %s: %v", t.ID, err)
		}
	}
	ctx, cancel := s.withTimeout(context.Background(), shutdownGrace)
	defer cancel()
	workers, stopWorkers := s.withTimeout(ctx, shutdownGrace-finalFlushGrace)
	defer stopWorkers()
	if !s.awaitWorkers(workers) {
		log.Printf("shutdown: %d requests still running", s.admit.Active())
	}
	if s.out.Submitting() {
		// The frames of a submit still in flight must not be sent twice.
		log.Printf("final submit: previous submit still running")
		return
	}
	for drained := false; !drained; {
		select {
		case f := <-s.replies:
			s.out.Take(f, s.admit.Canceled(f.RequestId))
		default:
			drained = true
		}
	}
	if err := s.submit(ctx); err != nil {
		log.Printf("final submit: %v", err)
	}
}

// awaitWorkers collects results until all workers and the submit in flight
// finish. It reports false if ctx expired first.
func (s *session) awaitWorkers(ctx context.Context) bool {
	for s.admit.Active() > 0 || s.out.Submitting() {
		select {
		case f := <-s.replies:
			if !s.out.Take(f, s.admit.Canceled(f.RequestId)) || s.out.Submitting() {
				continue
			}
			if err := s.submit(ctx); err != nil {
				log.Printf("submit: %v", err)
			}
		case r := <-s.submitted:
			s.out.Settle(r.frames, r.accepted)
		case id := <-s.done:
			s.admit.Finish(id)
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// submit delivers the pending frames on the caller's goroutine, retrying
// briefly on failure.
func (s *session) submit(ctx context.Context) error {
	failures := 0
	for len(s.out.Pending()) > 0 && s.cred.Current().Credential != "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		before := len(s.out.Pending())
		batch := s.out.Batch()
		credential := s.cred.Current().Credential
		n, err := s.pv.submitFrames(ctx, credential, batch)
		s.out.Accepted(batch, n)
		if err != nil {
			s.invalidate(credential, err)
			if s.cred.Current().Credential == "" {
				return err
			}
		}
		if len(s.out.Pending()) < before {
			failures = 0
			continue
		}
		failures++
		if failures >= replies.SubmitAttempts {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(failures) * submitRetryStep):
		}
	}
	return nil
}
