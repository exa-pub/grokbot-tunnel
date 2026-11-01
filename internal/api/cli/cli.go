// Package cli defines the grokbot-tunnel command line: flags and arguments
// become the options of one App call per command.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/sandapi"
	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/envvar"
	"github.com/exa-pub/grokbot-tunnel/internal/service/install"
	"github.com/exa-pub/grokbot-tunnel/internal/service/login"
	"github.com/exa-pub/grokbot-tunnel/internal/service/selfupdate"
)

// version is the release version, set at build time with
// -ldflags "-X github.com/exa-pub/grokbot-tunnel/internal/api/cli.version=v1.2.3".
var version = "dev"

// packager names the package manager that installed this build, set at
// build time like version. Such builds leave updates to it.
var packager = ""

// App runs the commands; app.App implements it.
type App interface {
	Login(ctx context.Context, opts LoginOptions, out io.Writer) error
	Connect(ctx context.Context, opts ConnectOptions) error
	Install(ctx context.Context, opts install.Options, out io.Writer) error
	Uninstall(ctx context.Context, out io.Writer) error
	Update(ctx context.Context, opts selfupdate.Options, out io.Writer) error
}

// LoginOptions configure App.Login.
type LoginOptions struct {
	login.Options
	ClientVersion string
}

// ConnectOptions configure App.Connect.
type ConnectOptions struct {
	Root          string // root for file tools and shell cwd; empty means home
	Permission    string // one of the entity.Mode* values
	Label         string // new computer label; empty keeps the saved one
	Debug         bool   // log raw requests
	ClientVersion string
	Version       string // grokbot-tunnel version, recorded in the instance lock
}

// Execute parses args and runs the selected command of a.
func Execute(ctx context.Context, args []string, a App) error {
	root := NewRoot(a)
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}

// ExitCode returns the exit status for an error from Execute:
// install.ExitLogin when the user has to log in again, which tells systemd
// that restarting cannot help, and 1 otherwise.
func ExitCode(err error) int {
	if errors.Is(err, entity.ErrLogout) || errors.Is(err, entity.ErrNotLoggedIn) {
		return install.ExitLogin
	}
	return 1
}

// NewRoot returns the root command running the commands of a.
func NewRoot(a App) *cobra.Command {
	var clientVersion string
	root := &cobra.Command{
		Use:           "grokbot-tunnel",
		Short:         "Run Grok Bot agent requests on this computer",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Runnable so that cobra.NoArgs rejects unknown subcommands.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return fmt.Errorf("missing command\n\n%s", cmd.UsageString())
		},
	}
	root.SetVersionTemplate("grokbot-tunnel {{.Version}} (Sand protocol " + sandapi.Version + ")\n")
	root.PersistentFlags().StringVar(&clientVersion, "client-version",
		envvar.Get("SAND_CLIENT_APP_VERSION", sandapi.Version), "Sand client version header")
	root.AddCommand(
		newLoginCommand(a, &clientVersion),
		newConnectCommand(a, &clientVersion),
		newInstallCommand(a, &clientVersion),
		newUninstallCommand(a),
		newUpdateCommand(a),
	)
	return root
}

func newLoginCommand(a App, clientVersion *string) *cobra.Command {
	var opts LoginOptions
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Sign in through the browser",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.ClientVersion = *clientVersion
			return a.Login(cmd.Context(), opts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().BoolVar(&opts.NoBrowser, "no-browser", false, "print login URL without opening browser")
	cmd.Flags().StringVar(&opts.TeamID, "team", "", "selected team ID")
	return cmd
}

func newConnectCommand(a App, clientVersion *string) *cobra.Command {
	var opts ConnectOptions
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Execute agent requests on this computer until interrupted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.ClientVersion = *clientVersion
			opts.Version = version
			return a.Connect(cmd.Context(), opts)
		},
	}
	connectFlags(cmd.Flags(), &opts.Root, &opts.Permission, &opts.Label, &opts.Debug, string(entity.ModeAsk))
	return cmd
}

// connectFlags defines the flags that configure connect, shared by connect
// and install.
func connectFlags(f *pflag.FlagSet, root, permission, label *string, debug *bool, defaultPermission string) {
	f.StringVar(root, "root", "", "root for file tools and shell working directories (default: home directory)")
	f.StringVar(permission, "permission", defaultPermission, "ask, always, or never")
	f.StringVar(label, "label", "", "computer label")
	f.BoolVar(debug, "debug", false, "log raw request JSON (may contain sensitive data)")
}

func newInstallCommand(a App, clientVersion *string) *cobra.Command {
	var opts install.Options
	var noAutoUpdate bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Run connect as a systemd service that starts at boot",
		Long: `Installs a systemd service that runs "grokbot-tunnel connect" with the given
flags, starts it at boot and restarts it on failure. Root gets a system
service; other users get a service of their own systemd user manager, with
lingering enabled so that it runs without a login session.

Unless --no-auto-update is given, a daily timer updates grokbot-tunnel and
restarts the service. Run install again to change the flags.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("client-version") {
				opts.ClientVersion = *clientVersion
			}
			opts.Version = version
			opts.AutoUpdate = !noAutoUpdate && packager == ""
			return a.Install(cmd.Context(), opts, cmd.OutOrStdout())
		},
	}
	connectFlags(cmd.Flags(), &opts.Root, &opts.Permission, &opts.Label, &opts.Debug, "")
	cmd.Flags().Lookup("permission").Usage = "ask, always, or never (required)"
	cmd.Flags().BoolVar(&noAutoUpdate, "no-auto-update", false, "do not update grokbot-tunnel daily")
	return cmd
}

func newUninstallCommand(a App) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the systemd service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.Uninstall(cmd.Context(), cmd.OutOrStdout())
		},
	}
}

func newUpdateCommand(a App) *cobra.Command {
	var opts selfupdate.Options
	cmd := &cobra.Command{
		Use:   "update [VERSION]",
		Short: "Update grokbot-tunnel to the latest or the given release",
		Long: `Downloads a release from GitHub, verifies its checksum, replaces this binary
and restarts the service if it runs this binary. VERSION is "latest" (the
default) or a release such as v1.2.3.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.Target = "latest"
			if len(args) == 1 {
				opts.Target = args[0]
			}
			opts.Version = version
			opts.Packager = packager
			return a.Update(cmd.Context(), opts, cmd.OutOrStdout())
		},
	}
	f := cmd.Flags()
	f.BoolVar(&opts.Check, "check", false, "only show the installed and the available version")
	f.BoolVar(&opts.Force, "force", false, "reinstall the same version, downgrade, or replace a development build")
	f.BoolVar(&opts.NoRestart, "no-restart", false, "do not restart the service")
	f.BoolVar(&opts.Auto, "auto", false, "run by the update timer: skip instead of failing when a decision is needed")
	if err := f.MarkHidden("auto"); err != nil {
		panic(err)
	}
	return cmd
}
