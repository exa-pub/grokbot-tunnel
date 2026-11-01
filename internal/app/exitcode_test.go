package app

import (
	"errors"
	"fmt"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer"
	"github.com/exa-pub/grokbot-tunnel/internal/service/install"
)

// A computer removed from the account exits with the status that keeps
// systemd from restarting the service, like a revoked login.
func TestExitCodeRemoved(t *testing.T) {
	for err, want := range map[error]int{
		fmt.Errorf("%w (failed_precondition)", computer.ErrRemoved): install.ExitLogin,
		entity.ErrLogout:      install.ExitLogin,
		errors.New("network"): 1,
	} {
		if got := ExitCode(err); got != want {
			t.Errorf("ExitCode(%v) = %d, want %d", err, got, want)
		}
	}
}
