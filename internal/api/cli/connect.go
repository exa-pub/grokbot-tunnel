package cli

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
)

func newConnectCommand(a App, clientVersion *string) *cobra.Command {
	var opts ConnectOptions
	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Execute agent requests on this computer until interrupted",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.ClientVersion = *clientVersion
			opts.Version = version
			var err error
			if opts.Label, err = normalizeLabel(opts.Label); err != nil {
				return err
			}
			return a.Connect(cmd.Context(), opts)
		},
	}
	connectFlags(cmd.Flags(), &opts.Root, &opts.Permission, &opts.Label, &opts.Debug, string(entity.ModeAsk))
	return cmd
}

// connectFlags defines the flags that configure connect, shared by connect
// and install.
func connectFlags(f *pflag.FlagSet, root, permission, label *string, debug *bool, defaultPermission string) {
	f.StringVar(root, "root", "", "root for file tools and default directory of commands (default: home directory)")
	f.StringVar(permission, "permission", defaultPermission, "ask, always, or never")
	f.StringVar(label, "label", "", "computer label, at most 100 characters (default: host name)")
	f.BoolVar(debug, "debug", false, "log raw request JSON (may contain sensitive data)")
}

// normalizeLabel trims a --label and cuts it to the length the desktop
// allows. Empty keeps the saved label; a blank one is refused.
func normalizeLabel(label string) (string, error) {
	if label == "" {
		return "", nil
	}
	if label = entity.NormalizeLabel(label); label == "" {
		return "", errors.New("--label must not be blank")
	}
	return label, nil
}
