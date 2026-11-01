package cli

import (
	"github.com/spf13/cobra"

	"github.com/exa-pub/grokbot-tunnel/internal/service/install"
)

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
			var err error
			if opts.Label, err = normalizeLabel(opts.Label); err != nil {
				return err
			}
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
