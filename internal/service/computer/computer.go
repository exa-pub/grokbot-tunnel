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
	"fmt"
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

const rpcTimeout = 20 * time.Second

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
	Standing     func() entity.Mode
	Debug        bool
	Backend      string
	AccountScope string
	Now          func() time.Time
	Jitter       func(time.Duration) time.Duration
}

// Run serves requests until ctx is canceled or a fatal error occurs.
func (pv *Provider) Run(ctx context.Context) error {
	s := newSession(ctx, pv)
	defer s.close()
	return s.run(ctx)
}

func rpcContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, rpcTimeout)
}

func (pv *Provider) issue(ctx context.Context) (entity.Credential, error) {
	ctx, cancel := rpcContext(ctx)
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

// forgetCredential overwrites the cached credential after the server rejected
// it.
func (pv *Provider) forgetCredential() {
	if pv.Store == nil {
		return
	}
	if err := pv.Store.Save("computer", entity.Credential{}); err != nil {
		log.Printf("forget computer credential: %v", err)
	}
}

// submitFrames sends one batch and returns how many frames the server
// accepted. It does not touch session state, so it may run on any goroutine.
func (pv *Provider) submitFrames(ctx context.Context, credential string, frames []*responseFrame) (int, error) {
	ctx, cancel := rpcContext(ctx)
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
	n := int(r.Msg.AcceptedCount)
	if n > len(frames) {
		return 0, fmt.Errorf("invalid accepted_count %d", n)
	}
	if n < len(frames) {
		return n, fmt.Errorf("server accepted only %d response frames", n)
	}
	return n, nil
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
