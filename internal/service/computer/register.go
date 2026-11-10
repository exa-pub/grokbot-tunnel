package computer

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
)

// registerTimeout bounds each call of Register, like the desktop's zae and
// its privacy check (timeoutMs 1e4).
const registerTimeout = 10 * time.Second

// Register announces this machine to the user's account under label, with
// the local permission mode, before Run serves it. Like the desktop
// ("Machine privacy check"), it first asks for the privacy mode of the
// selected team, team, and registers only if that mode lets Grok Bot keep
// data; it reports whether it registered. A computer that is not registered
// is left to the server to accept or refuse.
func Register(ctx context.Context, dashboard rpc.DashboardServiceClient, label string, mode entity.Mode, team string) (bool, error) {
	allowed, err := storageAllowed(ctx, dashboard, team)
	if err != nil || !allowed {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()
	permission := string(mode)
	req := &aiserverv1.RegisterSandMachineRequest{Label: label, LocalToolPermission: &permission}
	if _, err := dashboard.RegisterSandMachine(ctx, connect.NewRequest(req)); err != nil {
		return false, fmt.Errorf("register computer: %w", err)
	}
	return true, nil
}

// storageAllowed reports whether the privacy mode of team, or of the account
// if no team is selected, allows storage: no training, or a mode that
// allows training (the desktop's Jne).
func storageAllowed(ctx context.Context, dashboard rpc.DashboardServiceClient, team string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, registerTimeout)
	defer cancel()
	req := &aiserverv1.GetUserPrivacyModeRequest{}
	// The desktop names a team only by a positive ID.
	if id, err := strconv.ParseInt(team, 10, 32); err == nil && id > 0 {
		req.TeamId = proto.Int32(int32(id))
	}
	r, err := dashboard.GetUserPrivacyMode(ctx, connect.NewRequest(req))
	if err != nil {
		return false, fmt.Errorf("check the account's privacy mode: %w", err)
	}
	switch r.Msg.GetPrivacyMode() {
	case aiserverv1.PrivacyMode_PRIVACY_MODE_NO_TRAINING,
		aiserverv1.PrivacyMode_PRIVACY_MODE_USAGE_DATA_TRAINING_ALLOWED,
		aiserverv1.PrivacyMode_PRIVACY_MODE_USAGE_CODEBASE_TRAINING_ALLOWED:
		return true, nil
	default:
		return false, nil
	}
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
