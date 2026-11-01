package app

import (
	"context"
	"io"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/api/cli"
	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/login"
)

// Login signs the user in through the browser; see login.Login.
func Login(ctx context.Context, opts cli.LoginOptions, out io.Writer) error {
	if err := opts.Validate(); err != nil {
		return err
	}
	e, err := newEnv(opts.ClientVersion)
	if err != nil {
		return err
	}
	l := login.Login{Store: e.store, HTTP: e.rest, Base: e.base, Website: websiteURL(),
		Profile: func(ctx context.Context, identity entity.Identity) (entity.Identity, error) {
			u := e.user(identity)
			me, err := u.dashboard.GetMe(ctx, connect.NewRequest(&aiserverv1.GetMeRequest{}))
			// A refresh GetMe started outlives it: wait, so that its answer
			// is saved before login exits and is in the identity below.
			awaitRefresh(ctx, u.tokens)
			if err != nil {
				return identity, err
			}
			identity = u.tokens.Snapshot()
			identity.Email = me.Msg.GetEmail()
			identity.AuthID = me.Msg.GetAuthId()
			return identity, nil
		},
	}
	return l.Run(ctx, opts.Options, out)
}
