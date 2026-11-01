package cli

import (
	"github.com/spf13/cobra"

	"github.com/exa-pub/grokbot-tunnel/internal/service/selfupdate"
)

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
