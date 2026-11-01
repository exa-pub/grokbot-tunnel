package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
)

func TestExitCode(t *testing.T) {
	for err, want := range map[error]int{
		errors.New("boom"):                           1,
		entity.ErrNotLoggedIn:                        78,
		fmt.Errorf("register: %w", entity.ErrLogout): 78,
	} {
		if got := ExitCode(err); got != want {
			t.Errorf("ExitCode(%v) = %d, want %d", err, got, want)
		}
	}
}
