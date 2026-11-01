package computer

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
)

// Register announces this machine to the user's account under label, with
// the local permission mode, before Run serves it.
func Register(ctx context.Context, dashboard rpc.DashboardServiceClient, label string, mode entity.Mode) error {
	ctx, cancel := rpcContext(ctx)
	defer cancel()
	permission := string(mode)
	req := &aiserverv1.RegisterSandMachineRequest{Label: label, LocalToolPermission: &permission}
	if _, err := dashboard.RegisterSandMachine(ctx, connect.NewRequest(req)); err != nil {
		return fmt.Errorf("register computer: %w", err)
	}
	return nil
}
