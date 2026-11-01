package computer

import (
	"context"
	"log"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
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

// authorityNotice returns what to tell the user when Grok Bot takes over
// the permission decisions for this computer with credential c, or hands
// them back, and "" otherwise. In server-authoritative mode --permission has
// no effect: the server approves or refuses each action and shows its own
// mode, so the user would not learn otherwise why "ask" never asks.
func (s *session) authorityNotice(c entity.Credential) string {
	if c.Credential == "" || s.authorityKnown && s.serverAuthority == c.ServerAuthoritative {
		return ""
	}
	first := !s.authorityKnown
	s.authorityKnown, s.serverAuthority = true, c.ServerAuthoritative
	mode := ""
	if s.pv.Standing != nil {
		mode = " " + string(s.pv.Standing())
	}
	switch {
	case c.ServerAuthoritative && s.pv.ServerPermission != "":
		return "Grok Bot manages permissions for this computer: it decides each action by its setting, " +
			"set to " + string(s.pv.ServerPermission) + " from --permission at start"
	case c.ServerAuthoritative:
		return "Grok Bot manages permissions for this computer: it decides each action by its setting, " +
			"and --permission" + mode + " has no effect; change the setting in Grok Bot, " +
			"or give --permission to set it at start"
	case !first:
		return "Grok Bot no longer manages permissions for this computer: --permission" + mode + " applies"
	}
	return ""
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
	if notice := s.authorityNotice(c); notice != "" {
		log.Print(notice)
	}
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
