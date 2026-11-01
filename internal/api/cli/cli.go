// Package cli defines the grokbot-tunnel command line: flags and arguments
// become the options of one App call per command.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

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
	Root       string // root for file tools and shell cwd; empty means home
	Permission string // one of the entity.Mode* values
	// PermissionSet is whether --permission was given; only then is it
	// also set in Grok Bot, so a plain connect does not reset that to ask.
	PermissionSet bool
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
