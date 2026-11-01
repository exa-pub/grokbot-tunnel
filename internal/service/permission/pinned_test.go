package permission

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
)

// Invariant 17 of ai.tmp/refactor-plan/grok-bot-map.md §4, ≠ (decided: align
// with the desktop). Pins how a request is decided without a prompt today: a
// local "never" wins even over a server-authorized request. On the desktop a
// server-authoritative request is decided by the server alone, so the first
// row becomes (true, nil) in the alignment PR.
func TestPinnedDecide(t *testing.T) {
	for _, tc := range []struct {
		mode             entity.Mode
		server, approved bool
		ok               bool
		err              error
	}{
		{entity.ModeNever, true, true, true, ErrDisabled},
		{entity.ModeNever, true, false, true, ErrDisabled},
		{entity.ModeNever, false, false, true, ErrDisabled},
		{entity.ModeAsk, true, true, true, nil},
		{entity.ModeAsk, true, false, true, ErrRefused},
		{entity.ModeAlways, true, false, true, ErrRefused},
		{entity.ModeAlways, false, false, true, nil},
		{entity.ModeAsk, false, false, false, nil},
	} {
		p := &Policy{Mode: tc.mode}
		ok, err := p.decide(tc.server, tc.approved)
		assert.Equal(t, tc.ok, ok, "%+v", tc)
		assert.ErrorIs(t, err, tc.err, "%+v", tc)
	}
}
