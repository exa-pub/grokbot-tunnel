package computer

import (
	"context"
	"log"

	"connectrpc.com/connect"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

func (s *session) onIssued(r issued) error {
	if err := s.cred.Issued(s.pv.now(), r.credential, r.err); err != nil {
		return err
	}
	if r.err != nil {
		if s.work.Err() == nil {
			log.Printf("credential: %v", r.err)
		}
		return nil
	}
	// The next tick dials with the new credential once the loop allows; the
	// new credential polls at once.
	s.stopWatch()
	s.poll.Reset()
	return nil
}

// stopWatch cancels the watch stream and ignores its cancellation event.
func (s *session) stopWatch() {
	if s.watchCancel != nil {
		s.watchCancel()
	}
	s.stream.Stop()
}

// startWatch replaces the watch stream with a new one for the current
// credential.
func (s *session) startWatch() {
	if s.watchCancel != nil {
		s.watchCancel()
	}
	c := s.cred.Current()
	standing := s.pv.standing(c)
	gen := s.stream.Start(s.pv.now(), standing)
	ctx, cancel := context.WithCancel(s.work)
	s.watchCancel = cancel
	req := &aiserverv1.WatchGrokBotUserComputerRequestsRequest{
		MachineId:     s.pv.Machine.MachineID,
		Credential:    c.Credential,
		Hello:         s.pv.hello(c, standing),
		PresenceToken: s.presence,
	}
	go func() {
		err := func() error {
			stream, err := s.pv.Channel.WatchGrokBotUserComputerRequests(ctx, connect.NewRequest(req))
			if err != nil {
				return err
			}
			defer stream.Close()
			for stream.Receive() {
				select {
				case s.events <- watched{event: stream.Msg(), generation: gen}:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return stream.Err()
		}()
		select {
		case s.events <- watched{err: err, generation: gen}:
		case <-ctx.Done():
		}
	}()
}

// invalidate drops the credential if err shows the server rejected it; a
// result for credential, if given, says nothing about a newer one.
func (s *session) invalidate(credential string, err error) {
	rejected := s.cred.Current().Credential
	if !s.cred.Reject(s.pv.now(), credential, err) {
		return
	}
	s.pv.forgetCredential(rejected)
	s.stopWatch()
	s.stream.DelayDial(s.pv.now(), channelRetry)
}
