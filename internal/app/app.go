// Package app assembles the program: it reads the environment, builds the
// HTTP and RPC clients, the store and the services, and hands them to the
// command line. It holds no logic of its own.
package app

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/githubrelease"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/oauth"
	"github.com/exa-pub/grokbot-tunnel/internal/adapter/sandapi"
	"github.com/exa-pub/grokbot-tunnel/internal/adapter/systemd"
	"github.com/exa-pub/grokbot-tunnel/internal/api/cli"
	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/staterepo"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer"
	"github.com/exa-pub/grokbot-tunnel/internal/service/install"
	"github.com/exa-pub/grokbot-tunnel/internal/service/selfupdate"
	"github.com/exa-pub/grokbot-tunnel/internal/service/tokens"
)

const (
	restTimeout = 20 * time.Second
	maxMessage  = 128 << 20
)

// App implements the commands of the command line.
type App struct{}

var _ cli.App = App{}

// Run runs the command line with args.
func Run(ctx context.Context, args []string) error {
	if systemd.Journald() {
		// The journal stamps every line itself.
		log.SetFlags(0)
	}
	return cli.Execute(ctx, args, App{})
}

// ExitCode returns the exit status for an error from Run. A computer removed
// from the account exits like a revoked login, so that systemd does not
// restart the service, which would register the computer again.
func ExitCode(err error) int {
	if errors.Is(err, computer.ErrRemoved) {
		return install.ExitLogin
	}
	return cli.ExitCode(err)
}

// Login signs the user in; see Login.
func (App) Login(ctx context.Context, opts cli.LoginOptions, out io.Writer) error {
	return Login(ctx, opts, out)
}

// Connect serves requests; see Connect.
func (App) Connect(ctx context.Context, opts cli.ConnectOptions) error { return Connect(ctx, opts) }

// Install installs the service; see install.Install.
func (App) Install(ctx context.Context, opts install.Options, out io.Writer) error {
	return install.Install(ctx, opts, out)
}

// Uninstall removes the service; see install.Uninstall.
func (App) Uninstall(ctx context.Context, out io.Writer) error { return install.Uninstall(ctx, out) }

// Update replaces this binary; see selfupdate.Run.
func (App) Update(ctx context.Context, opts selfupdate.Options, out io.Writer) error {
	opts.Unit = install.TunnelUnit
	if url := releasesURL(); url != "" {
		opts.Client = &githubrelease.Client{Base: url}
	}
	return selfupdate.Run(ctx, opts, out)
}

// env holds what both workflows need.
type env struct {
	store    *staterepo.Store
	machine  entity.Machine
	base     string
	rest     *http.Client // plain REST calls: login polling and token refresh
	computer *http.Client // Connect RPC authenticated by the computer credential
	rpc      *http.Client // Connect RPC authenticated as the user
	lookup   *http.Client // privacy-mode lookups, as the user but always private
	privacy  *sandapi.Privacy
}

func newEnv(clientVersion string) (*env, error) {
	store, err := staterepo.Default()
	if err != nil {
		return nil, err
	}
	machine, err := store.Machine()
	if err != nil {
		return nil, err
	}
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("http.DefaultTransport is not an *http.Transport")
	}
	// Like the desktop's clients, all of them speak HTTP/1.1 only.
	http1 := sandapi.HTTP1(defaultTransport)
	privacy := &sandapi.Privacy{}
	userRPC := sandapi.Transport{
		Base:    http1,
		Machine: machine.MachineID,
		Version: clientVersion,
		Profile: sandapi.UserRPC,
		Privacy: privacy,
	}
	return &env{
		store:   store,
		machine: machine,
		base:    backendURL(),
		rest: &http.Client{Timeout: restTimeout, Transport: sandapi.Transport{
			Base:    http1.Clone(),
			Profile: sandapi.AuthREST,
		}},
		computer: &http.Client{Transport: sandapi.Transport{
			Base:    http1.Clone(),
			Version: clientVersion,
			Profile: sandapi.ComputerRPC,
		}},
		rpc:     &http.Client{Transport: userRPC},
		lookup:  sandapi.PrivacyHTTP(userRPC),
		privacy: privacy,
	}, nil
}

// user holds the RPC clients authenticated as the signed-in user.
type user struct {
	tokens    *tokens.TokenSource
	dashboard rpc.DashboardServiceClient
	grokbot   rpc.GrokBotServiceClient
}

func (e *env) user(identity entity.Identity) *user {
	tokens := &tokens.TokenSource{
		Store: e.store, HTTP: e.rest, Base: e.base, ClientID: oauth.ClientIDFor(e.base, authClientID()), Identity: identity,
	}
	u := &user{
		tokens:    tokens,
		dashboard: rpc.NewDashboardServiceClient(e.rpc, e.base, userOptions(tokens)...),
		grokbot:   rpc.NewGrokBotServiceClient(e.rpc, e.base, userOptions(tokens)...),
	}
	// Ghost mode depends on the account, so look it up as this user.
	e.privacy.Client = rpc.NewDashboardServiceClient(e.lookup, e.base, userOptions(tokens)...)
	return u
}

// computerChannel returns the client for the credential-authenticated channel.
func (e *env) computerChannel() rpc.GrokBotServiceClient {
	return rpc.NewGrokBotServiceClient(e.computer, e.base, connect.WithReadMaxBytes(maxMessage), sandapi.Compression())
}

// userOptions authenticate calls with the user's bearer token.
func userOptions(tokens *tokens.TokenSource) []connect.ClientOption {
	return []connect.ClientOption{
		connect.WithInterceptors(sandapi.Bearer(tokens)),
		connect.WithReadMaxBytes(maxMessage),
		sandapi.Compression(),
	}
}
