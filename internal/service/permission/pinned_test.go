package permission

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
)

// Invariant 17: how a request is decided without a prompt. Under
// server-authoritative mode the server decides alone, as on the desktop, so a
// local "never" does not refuse what the server approved (PLAN.md §9).
func TestPinnedDecide(t *testing.T) {
	for _, tc := range []struct {
		mode             entity.Mode
		server, approved bool
		ok               bool
		err              error
	}{
		{entity.ModeNever, true, true, true, nil},
		{entity.ModeNever, true, false, true, ErrRefused},
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
