package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/systemd"
	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/atomicfile"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/instancelock"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/staterepo"
)

// Options configure Install: the connect flags of the service, and how it
// is installed.
type Options struct {
	Permission    string // required
	Root          string
	Label         string
	Debug         bool
	ClientVersion string // set only if the user chose one
	AutoUpdate    bool   // install the daily update timer
	Version       string // version of this binary
}

// errPermissionRequired explains the choice instead of defaulting to "ask",
// which refuses nearly everything without a terminal, or to "always".
var errPermissionRequired = errors.New("--permission is required: " +
	"\"always\" runs every action the agent requests; " +
	"\"ask\" runs only actions approved in Grok Bot, since a service has no terminal to ask in; " +
	"\"never\" refuses all actions, unless Grok Bot manages permissions itself")

// networkEnv lists the variables through which install's environment
// reaches the server: Go's ProxyFromEnvironment reads the proxies (but not
// ALL_PROXY) and crypto/x509 the CA certificates. The desktop has proxy
// settings of its own (Dz C@443922, kz C@443811); a service started without
// these could not connect or update behind a proxy.
var networkEnv = []string{
	"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

// proxyURLs are the variables of networkEnv holding a proxy URL, which may
// carry credentials.
var proxyURLs = []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"}

// serviceEnv lists the variables the service keeps from the environment of
// install: the user manager starts services with a minimal PATH, the SAND_
// and CURSOR_ ones select the server, and networkEnv reaches it.
var serviceEnv = append([]string{
	"PATH", "SHELL",
	"SAND_BACKEND_URL", "SAND_CURSOR_WEBSITE_URL", "SAND_CLIENT_APP_VERSION", "SAND_AUTH_CLIENT_ID",
	"CURSOR_API_BASE_URL", "CURSOR_WEBSITE_URL",
}, networkEnv...)

// EnvFile is the file in the store that passes the services the variables
// with credentials: proxy URLs with a user. Environment= would put them in
// a world-readable unit file, and of a system unit systemctl show tells
// them to every user; the file is readable only by its owner and systemd
// shows only its path.
const EnvFile = "service.env"

// Install runs connect as a systemd service that starts at boot.
func Install(ctx context.Context, opts Options, out io.Writer) error {
	args, err := opts.connectArgs()
	if err != nil {
		return err
	}
	if systemd.UnderSudo() {
		return systemd.ErrSudo
	}
	m, err := serviceManager(ctx, out)
	if err != nil {
		return err
	}
	store, err := staterepo.Default()
	if err != nil {
		return err
	}
	if err = checkInstallable(store); err != nil {
		return err
	}
	exe, err := serviceExecutable()
	if err != nil {
		return err
	}
	c := Config{Exe: exe, Args: args, Version: opts.Version, AutoUpdate: opts.AutoUpdate}
	if err = keepEnv(&c, store, out); err != nil {
		return err
	}
	if opts.Permission == string(entity.ModeAsk) {
		fmt.Fprintln(out, "Note: with --permission ask the service has no terminal to ask in, "+
			"so it runs only actions approved in Grok Bot and refuses the rest.")
	}
	if err = installUnits(ctx, m, c); err != nil {
		return err
	}
	flag := m.Scope.Flag()
	fmt.Fprintf(out, "\nThe service is connected and starts at boot.\n"+
		"    systemctl %sstatus %s\n    journalctl %s-u %s -f\n    grokbot-tunnel uninstall\n",
		flag, Name, flag, Name)
	if opts.AutoUpdate {
		fmt.Fprintln(out, "It updates grokbot-tunnel daily; re-run install with --no-auto-update to turn that off.")
	}
	return nil
}

// keepEnv adds the variables of serviceEnv set for install to c and prints
// them, credentials masked. Those with credentials go to EnvFile in store,
// which is removed if there are none.
func keepEnv(c *Config, store *staterepo.Store, out io.Writer) error {
	var secret []string
	for _, k := range serviceEnv {
		v := os.Getenv(k)
		if v == "" {
			continue
		}
		fmt.Fprintf(out, "The service runs with %s=%s\n", k, maskProxy(k, v))
		if slices.Contains(proxyURLs, k) && strings.Contains(v, "@") {
			secret = append(secret, k+"="+v)
		} else {
			c.Env = append(c.Env, k+"="+v)
		}
	}
	path := filepath.Join(store.Dir, EnvFile)
	if len(secret) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	content, err := envFile(c.Version, secret)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(store.Dir, 0o700); err != nil {
		return err
	}
	if err = atomicfile.Write(path, []byte(content), atomicfile.Options{Mode: 0o600, Prefix: ".tmp-" + EnvFile + "-"}); err != nil {
		return err
	}
	c.EnvFile = path
	fmt.Fprintf(out, "The proxy credentials are kept in %s, readable only by its owner\n", path)
	return nil
}

// maskProxy masks the credentials in the value of a proxy URL variable k for
// printing: a password becomes ***, and so does a user without one, which
// may be a token. Go takes proxy URLs without a scheme too.
func maskProxy(k, v string) string {
	at := strings.LastIndex(v, "@")
	if !slices.Contains(proxyURLs, k) || at < 0 {
		return v
	}
	start := 0
	if i := strings.Index(v[:at], "://"); i >= 0 {
		start = i + len("://")
	}
	user, _, ok := strings.Cut(v[start:at], ":")
	if !ok {
		return v[:start] + "***" + v[at:]
	}
	return v[:start] + user + ":***" + v[at:]
}

// connectArgs validates the connect flags and renders them for ExecStart.
func (o Options) connectArgs() ([]string, error) {
	c := o
	if c.Permission == "" {
		return nil, errPermissionRequired
	}
	if _, err := entity.ParseMode(c.Permission); err != nil {
		return nil, fmt.Errorf("invalid permission %q", c.Permission)
	}
	args := []string{"--permission", c.Permission}
	if c.Root != "" {
		root, err := filepath.Abs(c.Root)
		if err != nil {
			return nil, err
		}
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			return nil, fmt.Errorf("--root %s is not a directory", root)
		}
		args = append(args, "--root", root)
	}
	if c.Label != "" {
		args = append(args, "--label", c.Label)
	}
	if c.Debug {
		args = append(args, "--debug")
	}
	if c.ClientVersion != "" {
		args = append(args, "--client-version", c.ClientVersion)
	}
	return args, nil
}

// serviceManager returns the manager for the current user's scope after
// checking that systemd is there to talk to.
func serviceManager(ctx context.Context, out io.Writer) (*systemd.Manager, error) {
	if err := systemd.Booted(); err != nil {
		return nil, err
	}
	scope, err := systemd.DetectScope()
	if err != nil {
		return nil, err
	}
	m := systemd.New(scope, out)
	return m, m.Reachable(ctx)
}

// checkInstallable fails if the service could not connect: without a login,
// or while a connect outside the service holds the instance lock.
func checkInstallable(store *staterepo.Store) error {
	if _, err := store.Identity(); errors.Is(err, entity.ErrNotLoggedIn) {
		return errors.New("not logged in; run \"grokbot-tunnel login\" first")
	} else if err != nil {
		return err
	}
	h, held, err := instancelock.Probe(store.Dir, "connect")
	if err != nil || !held {
		return err
	}
	if _, ok := systemd.Owner(h.PID, TunnelUnit); ok {
		return nil // the service itself; installing restarts it
	}
	return fmt.Errorf("grokbot-tunnel connect is already running (pid %d); stop it first, since only one can run per user", h.PID)
}

// serviceExecutable returns the binary the service runs, refusing temporary
// builds that would disappear.
func serviceExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	tmp := filepath.Clean(os.TempDir()) + string(filepath.Separator)
	if strings.HasPrefix(exe, tmp) || strings.Contains(exe, "/go-build") {
		return "", fmt.Errorf("%s is a temporary build; install grokbot-tunnel and run install from the installed binary", exe)
	}
	return exe, nil
}

// Uninstall stops and removes the service. The binary and the login stay.
func Uninstall(ctx context.Context, out io.Writer) error {
	m, err := serviceManager(ctx, out)
	if err != nil {
		return err
	}
	store, err := staterepo.Default()
	if err != nil {
		return err
	}
	removed, err := removeUnits(ctx, m, filepath.Join(store.Dir, EnvFile))
	if err != nil {
		return err
	}
	if !removed {
		if _, err := os.Stat(filepath.Join(systemd.SystemDir, TunnelUnit)); err == nil && m.Scope.User {
			fmt.Fprintln(out, "The service is installed for root; run \"grokbot-tunnel uninstall\" as root to remove it.")
			return nil
		}
		fmt.Fprintln(out, "The service is not installed.")
		return nil
	}
	fmt.Fprintln(out, "Uninstalled the service. The grokbot-tunnel binary and your login in ~/.grok/bot-tunnel were kept.")
	if m.Scope.User {
		fmt.Fprintln(out, "Lingering stays enabled for other user services; turn it off with: loginctl disable-linger")
	}
	return nil
}
