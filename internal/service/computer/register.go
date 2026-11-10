package computer

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
)

// registerTimeout bounds RegisterSandMachine, like the desktop's zae.
const registerTimeout = 10 * time.Second

// Register announces this machine to the user's account under label, with
// the local permission mode, before Run serves it. Like the desktop's
// registration (timeoutMs 1e4), it gives up after registerTimeout.
func Register(ctx context.Context, dashboard rpc.DashboardServiceClient, label string, mode entity.Mode) error {
	ctx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()
	permission := string(mode)
	req := &aiserverv1.RegisterSandMachineRequest{Label: label, LocalToolPermission: &permission}
	if _, err := dashboard.RegisterSandMachine(ctx, connect.NewRequest(req)); err != nil {
		return fmt.Errorf("register computer: %w", err)
	}
	return nil
}

// SetPermission sets the permission mode that Grok Bot keeps for this
// computer, machineID, and shows in its settings, like the desktop when the
// mode of a machine is changed there (updateSandMachineLocalToolPermission).
// Where Grok Bot manages permissions itself, that mode decides instead of
// the tunnel's, and RegisterSandMachine, which carries the mode too, does not
// change it for a computer registered before. It returns the mode Grok Bot
// keeps, which a team's ceiling can lower.
func SetPermission(ctx context.Context, dashboard rpc.DashboardServiceClient, machineID string, mode entity.Mode) (entity.Mode, error) {
	ctx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()
	req := &aiserverv1.UpdateSandMachineLocalToolPermissionRequest{MachineId: machineID, LocalToolPermission: string(mode)}
	r, err := dashboard.UpdateSandMachineLocalToolPermission(ctx, connect.NewRequest(req))
	if err != nil {
		return "", fmt.Errorf("set the computer's permission in Grok Bot: %w", err)
	}
	kept := entity.Mode(r.Msg.GetMachine().GetLocalToolPermission())
	if _, err := entity.ParseMode(string(kept)); err != nil {
		return "", fmt.Errorf("set the computer's permission in Grok Bot: unexpected mode %q", kept)
	}
	return kept, nil
}
