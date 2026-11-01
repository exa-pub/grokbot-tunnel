package cli

import (
	"github.com/spf13/cobra"
)

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
