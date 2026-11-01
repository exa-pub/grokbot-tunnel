package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/systemd"
	"github.com/exa-pub/grokbot-tunnel/internal/adapter/tty"
	"github.com/exa-pub/grokbot-tunnel/internal/api/cli"
	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/instancelock"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/staterepo"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer"
	"github.com/exa-pub/grokbot-tunnel/internal/service/install"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec"
	"github.com/exa-pub/grokbot-tunnel/internal/service/permission"
)

// Validate checks the options without touching the system.
func validateConnect(o cli.ConnectOptions) error {
	if _, err := entity.ParseMode(o.Permission); err != nil {
		return fmt.Errorf("invalid permission %q", o.Permission)
	}
	return nil
}

// Connect registers this computer and executes the agent's requests until ctx
// is canceled. Only one Connect runs per store, since they would share the
// computer's identity.
func Connect(ctx context.Context, opts cli.ConnectOptions) error {
	if err := validateConnect(opts); err != nil {
		return err
	}
	notifier := systemd.NewNotifier()
	if opts.Root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		opts.Root = home
	}
	e, identity, release, err := openConnect(opts)
	if err != nil {
		return err
	}
	defer release()
	policy := &permission.Policy{Mode: entity.Mode(opts.Permission), Prompter: tty.Terminal{In: os.Stdin, Out: os.Stderr}}
	ex, err := localexec.New(opts.Root, policy.Allow)
	if err != nil {
		return err
	}
	defer ex.Close()

	u := e.user(identity)
	if err = register(ctx, u.dashboard, e.machine.Label, opts.Permission); err != nil {
		if ctx.Err() != nil {
			return nil // stopped while starting, e.g. by systemctl stop
		}
		return err
	}
	log.Printf("root=%s permission=%s", ex.Root, opts.Permission)
	notify(notifier, "READY=1")
	stopNotify := context.AfterFunc(ctx, func() { notify(notifier, "STOPPING=1") })
	defer stopNotify()
	pv := computer.Provider{
		User:            u.grokbot,
		Channel:         e.computerChannel(),
		Store:           e.store,
		Machine:         e.machine,
		Handler:         ex,
		LocalRoot:       ex.Root,
		TerminalsFolder: ex.TerminalsFolder(),
		Standing:        policy.Standing,
		Debug:           opts.Debug,
		Backend:         e.base,
		AccountScope:    u.tokens.Snapshot().AccountScope(),
	}
	return pv.Run(ctx)
}

// openConnect takes the instance lock and loads what Connect needs. The
// caller releases the lock.
func openConnect(opts cli.ConnectOptions) (*env, entity.Identity, func(), error) {
	store, err := staterepo.Default()
	if err != nil {
		return nil, entity.Identity{}, nil, err
	}
	// Before anything writes to the store, such as the label below.
	release, err := claimConnect(store, opts.Version)
	if err != nil {
		return nil, entity.Identity{}, nil, err
	}
	e, identity, err := loadConnect(opts)
	if err != nil {
		release()
		return nil, identity, nil, err
	}
	return e, identity, release, nil
}

func loadConnect(opts cli.ConnectOptions) (*env, entity.Identity, error) {
	e, err := newEnv(opts.ClientVersion)
	if err != nil {
		return nil, entity.Identity{}, err
	}
	warnCopiedStore(e)
	identity, err := e.store.Identity()
	if err != nil {
		return nil, identity, err
	}
	if opts.Label != "" {
		e.machine.Label = opts.Label
		if err = e.store.Save("machine", e.machine); err != nil {
			return nil, identity, err
		}
	}
	return e, identity, nil
}

// claimConnect takes the instance lock that keeps a second connect from
// serving the same computer.
func claimConnect(store *staterepo.Store, version string) (func(), error) {
	exe, err := os.Executable()
	if err != nil {
		exe = ""
	}
	self := instancelock.Holder{PID: os.Getpid(), Exe: exe, Version: version, StartedAt: time.Now().UTC()}
	release, err := instancelock.Claim(store.Dir, "connect", self)
	var busy *instancelock.BusyError
	if errors.As(err, &busy) {
		return nil, busyConnect(busy.Holder)
	}
	return release, err
}

func busyConnect(h instancelock.Holder) error {
	pid := "pid unknown"
	if h.PID != 0 {
		pid = fmt.Sprintf("pid %d", h.PID)
	}
	if scope, ok := systemd.Owner(h.PID, install.TunnelUnit); ok {
		return fmt.Errorf("the grokbot-tunnel service is already connected (%s); "+
			"stop it with \"systemctl %sstop %s\" or follow its log with \"journalctl %s-u %s -f\"",
			pid, scope.Flag(), install.Name, scope.Flag(), install.Name)
	}
	if !h.StartedAt.IsZero() {
		pid += ", started " + h.StartedAt.Local().Format(time.DateTime)
	}
	return fmt.Errorf("another grokbot-tunnel connect is already running (%s); only one can run per user", pid)
}

// warnCopiedStore warns if the store was created on another host: a copy
// connecting there would fight over the same computer, which the instance
// lock cannot see.
func warnCopiedStore(e *env) {
	if host := staterepo.HostID(); e.machine.HostID != "" && host != "" && e.machine.HostID != host {
		log.Printf("warning: %s was created on another host. If grokbot-tunnel connects there too, "+
			"both fight over the same computer in Grok Bot. Delete machine.json and computer.json "+
			"in it to register this host as a new computer.", e.store.Dir)
	}
}

func notify(n *systemd.Notifier, state string) {
	if err := n.Notify(state); err != nil {
		log.Printf("notify systemd: %v", err)
	}
}

func register(ctx context.Context, dashboard rpc.DashboardServiceClient, label, mode string) error {
	ctx, cancel := context.WithTimeout(ctx, restTimeout)
	defer cancel()
	req := &aiserverv1.RegisterSandMachineRequest{Label: label, LocalToolPermission: &mode}
	if _, err := dashboard.RegisterSandMachine(ctx, connect.NewRequest(req)); err != nil {
		return fmt.Errorf("register computer: %w", err)
	}
	return nil
}
