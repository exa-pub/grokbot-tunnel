// Package computer connects this machine to the server as a user computer,
// like the desktop's user-computer provider (log tag
// "[user-computer-provider]"): it keeps a credential, watches for requests,
// polls and runs them through a Handler, and submits their responses. The
// decisions live in the state machines of credential, watch, admission and
// replies; session runs the RPCs and goroutines around them.
package computer

import (
	"context"
	"errors"
	"log"
	"math/rand/v2"
	"time"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/staterepo"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/credential"
)

// Deadlines of the RPCs. Like the desktop provider's user-computer-data and
// user-computer-control deadlines, a submit attempt gets 120 s, enough for a
// batch of up to 8 MiB on a slow link, and a poll or a submit on shutdown
// 10 s. Issuing the credential gets userTimeout; Register has its own.
const (
	dataTimeout    = 120 * time.Second
	controlTimeout = 10 * time.Second
	userTimeout    = 20 * time.Second
)

// ErrRemoved ends Run when the computer was removed from the account after
// this process got a credential for it; connecting again registers it anew.
var ErrRemoved = credential.ErrRemoved

type (
	requestFrame  = aiserverv1.GrokBotUserComputerRequestFrame
	responseFrame = aiserverv1.GrokBotUserComputerResponseFrame
)

// Handler runs one request and streams its responses to sink; server selects
// server-authoritative permission checks. Failures are reported to sink; the
// returned error means sink itself failed. localexec.Executor implements it.
type Handler interface {
	Handle(ctx context.Context, f *requestFrame, server bool, sink frames.Sink) error
}

// Provider connects this machine to the server and executes its requests.
type Provider struct {
	User    rpc.GrokBotServiceClient // authenticated with the user's token
	Channel rpc.GrokBotServiceClient // authenticated with the computer credential
	Store   *staterepo.Store
	Machine entity.Machine
	Handler Handler
	// LocalRoot and TerminalsFolder are announced in the hello.
	LocalRoot       string
	TerminalsFolder string
	// Standing returns the local permission mode announced in the hello.
	Standing func() entity.Mode
	// ServerPermission is the mode set as this computer's permission in Grok
	// Bot at start (SetPermission), "" if it was not set.
	ServerPermission entity.Mode
	Debug            bool
	Backend          string
	AccountScope     string
	// OnConnected, if set, is called once, when the server first accepts the
	// computer on the watch stream.
	OnConnected func()
	// CloseIdle, if set, closes the idle connections of Channel. It is called
	// when the watch stream went silent, as the desktop renews its client
	// then, so that polls and submits do not reuse dead connections.
	CloseIdle func()
	Now       func() time.Time
	// Jitter, if set, replaces the random parts of retry delays, given their
	// bound d; tests make them predictable.
	Jitter func(d time.Duration) time.Duration
}

// Run serves requests until ctx is canceled or a fatal error occurs.
func (pv *Provider) Run(ctx context.Context) error {
	s := newSession(ctx, pv)
	defer s.close()
	return s.run(ctx)
}

func (pv *Provider) issue(ctx context.Context) (entity.Credential, error) {
	ctx, cancel := context.WithTimeout(ctx, userTimeout)
	defer cancel()
	req := &aiserverv1.IssueGrokBotUserComputerCredentialRequest{MachineId: pv.Machine.MachineID}
	r, err := pv.User.IssueGrokBotUserComputerCredential(ctx, connect.NewRequest(req))
	if err != nil {
		return entity.Credential{}, err
	}
	c := entity.Credential{
		AccountScope: pv.AccountScope, Backend: pv.Backend, MachineID: pv.Machine.MachineID,
		Credential:          r.Msg.Credential,
		ExpiresAtMS:         r.Msg.ExpiresAtMs,
		ServerAuthoritative: r.Msg.ServerAuthoritative,
	}
	if c.Credential == "" || c.ExpiresAtMS <= pv.now().UnixMilli() {
		return c, errors.New("server returned invalid computer credential")
	}
	// The file only caches the credential across restarts.
	if err := pv.Store.Save("computer", c); err != nil {
		log.Printf("cache computer credential: %v", err)
	}
	return c, nil
}

// cachedCredential returns the saved credential if it was issued for this
// account, backend and machine and is not about to expire.
func (pv *Provider) cachedCredential() entity.Credential {
	if pv.Store == nil || pv.Backend == "" || pv.AccountScope == "" {
		return entity.Credential{}
	}
	var c entity.Credential
	if err := pv.Store.Load("computer", &c); err != nil {
		return entity.Credential{}
	}
	sameScope := c.Backend == pv.Backend && c.AccountScope == pv.AccountScope && c.MachineID == pv.Machine.MachineID
	if !sameScope || time.UnixMilli(c.ExpiresAtMS).Sub(pv.now()) <= credential.Margin {
		return entity.Credential{}
	}
	return c
}

// forgetCredential removes the cached credential after the server rejected
// it, unless the cache already holds another one, e.g. issued by another
// process meanwhile (bug B9).
func (pv *Provider) forgetCredential(rejected string) {
	if pv.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if unlock, err := pv.Store.Lock(ctx); err == nil {
		defer unlock()
	}
	var cached entity.Credential
	if err := pv.Store.Load("computer", &cached); err != nil || cached.Credential != rejected {
		return
	}
	if err := pv.Store.Save("computer", entity.Credential{}); err != nil {
		log.Printf("forget computer credential: %v", err)
	}
}

// submitFrames sends one batch within timeout and returns how many frames
// are done with: all of them once the server answered. Like the desktop, a
// smaller accepted_count is only logged: the server does not say which
// frames it refused, and sending them again could repeat the final frame of
// a request. It does not touch session state, so it may run on any
// goroutine.
func (pv *Provider) submitFrames(ctx context.Context, timeout time.Duration, credential string, frames []*responseFrame) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req := &aiserverv1.SubmitGrokBotUserComputerResponsesRequest{
		MachineId:  pv.Machine.MachineID,
		Credential: credential,
		Frames:     frames,
	}
	r, err := pv.Channel.SubmitGrokBotUserComputerResponses(ctx, connect.NewRequest(req))
	if err != nil {
		return 0, err
	}
	if n := int(r.Msg.AcceptedCount); n < len(frames) {
		log.Printf("server accepted %d of %d response frames; the rest were not queued for this machine", n, len(frames))
	}
	return len(frames), nil
}

func (pv *Provider) now() time.Time {
	if pv.Now != nil {
		return pv.Now()
	}
	return time.Now()
}

func (pv *Provider) jitter(d time.Duration) time.Duration {
	if pv.Jitter != nil {
		return pv.Jitter(d)
	}
	return time.Duration(rand.Int64N(int64(max(d/2, time.Nanosecond)))) //nolint:gosec // jitter needs no cryptographic randomness
}

// random returns a random delay of at most d.
func (pv *Provider) random(d time.Duration) time.Duration {
	if pv.Jitter != nil {
		return pv.Jitter(d)
	}
	return time.Duration(rand.Int64N(int64(d) + 1)) //nolint:gosec // jitter needs no cryptographic randomness
}

// closeIdle closes the idle connections of the channel, if it can.
func (pv *Provider) closeIdle() {
	if pv.CloseIdle != nil {
		pv.CloseIdle()
	}
}
