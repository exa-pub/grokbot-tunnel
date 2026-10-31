package permission

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Invariant 17 of ai.tmp/refactor-plan/grok-bot-map.md §4, ≠ (decided: align
// with the desktop). Pins how a request is decided without a prompt today: a
// local "never" wins even over a server-authorized request. On the desktop a
// server-authoritative request is decided by the server alone, so the first
// row becomes (true, nil) in the alignment PR.
func TestPinnedDecide(t *testing.T) {
	for _, tc := range []struct {
		mode             string
		server, approved bool
		ok               bool
		err              error
	}{
		{ModeNever, true, true, true, ErrDisabled},
		{ModeNever, true, false, true, ErrDisabled},
		{ModeNever, false, false, true, ErrDisabled},
		{ModeAsk, true, true, true, nil},
		{ModeAsk, true, false, true, ErrRefused},
		{ModeAlways, true, false, true, ErrRefused},
		{ModeAlways, false, false, true, nil},
		{ModeAsk, false, false, false, nil},
	} {
		p := &Policy{Mode: tc.mode}
		ok, err := p.decide(tc.server, tc.approved)
		assert.Equal(t, tc.ok, ok, "%+v", tc)
		assert.ErrorIs(t, err, tc.err, "%+v", tc)
	}
}
