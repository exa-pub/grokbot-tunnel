package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/sandapi"
	"github.com/exa-pub/grokbot-tunnel/internal/adapter/systemd"
	"github.com/exa-pub/grokbot-tunnel/internal/adapter/tty"
	"github.com/exa-pub/grokbot-tunnel/internal/api/cli"
	"github.com/exa-pub/grokbot-tunnel/internal/entity"
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
	// On every way out, e.g. Register timing out while its refresh is still
	// waiting for the server's answer, or a stop in the middle of a refresh.
	defer awaitRefresh(ctx, u.tokens)
	registered, err := computer.Register(ctx, u.dashboard, e.machine.Label, entity.Mode(opts.Permission), identity.TeamID)
	if err != nil {
		if ctx.Err() != nil {
			return nil // stopped while starting, e.g. by systemctl stop
		}
		return err
	}
	if !registered {
		log.Print("the account's privacy mode does not allow storage: this computer is not registered with Grok Bot, which may refuse it")
	}
	var serverPermission entity.Mode
	if opts.PermissionSet {
		serverPermission = setServerPermission(ctx, u, e.machine.MachineID, entity.Mode(opts.Permission))
	}
	log.Printf("root=%s permission=%s", ex.Root, opts.Permission)
	stopNotify := context.AfterFunc(ctx, func() { notify(notifier, "STOPPING=1") })
	defer stopNotify()
	pv := computer.Provider{
		User:             u.grokbot,
		Channel:          e.computerChannel(),
		Store:            e.store,
		Machine:          e.machine,
		Handler:          ex,
		LocalRoot:        ex.Root,
		ServerPermission: serverPermission,
		TerminalsFolder:  ex.TerminalsFolder(),
		Standing:         policy.Standing,
		Debug:            opts.Debug,
		Backend:          e.base,
		AccountScope:     u.tokens.Snapshot().AccountScope(),
		// The service is up once the server accepts the computer, so that
		// install does not report a connection that fails afterwards.
		OnConnected: func() { notify(notifier, "READY=1") },
		CloseIdle:   closeIdle(e.computer),
	}
	return pv.Run(ctx)
}

// closeIdle returns a function closing the idle connections of c, whose
// transport sandapi.Transport wraps.
func closeIdle(c *http.Client) func() {
	if t, ok := c.Transport.(sandapi.Transport); ok {
		if base, ok := t.Base.(interface{ CloseIdleConnections() }); ok {
			return base.CloseIdleConnections
		}
	}
	return c.CloseIdleConnections
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

// setServerPermission sets mode, given with --permission, as the permission
// of this computer in Grok Bot, which decides by it where it manages
// permissions itself, and returns the mode Grok Bot keeps; "" if that failed,
// which does not stop the tunnel: the mode then stays what Grok Bot had.
func setServerPermission(ctx context.Context, u *user, machineID string, mode entity.Mode) entity.Mode {
	kept, err := computer.SetPermission(ctx, u.dashboard, machineID, mode)
	switch {
	case err != nil:
		if ctx.Err() == nil {
			log.Printf("%v; Grok Bot keeps the permission set there", err)
		}
		return ""
	case kept != mode:
		log.Printf("Grok Bot keeps permission %s for this computer instead of %s, e.g. by a team policy", kept, mode)
	}
	return kept
}
