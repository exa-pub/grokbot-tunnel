package credential_test

import (
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/exa-pub/grokbot-tunnel/internal/auth"
	"github.com/exa-pub/grokbot-tunnel/internal/config"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/credential"
)

func noJitter(time.Duration) time.Duration { return 0 }

func valid(now time.Time, ttl time.Duration, name string) config.Computer {
	return config.Computer{Credential: name, ExpiresAtMS: now.Add(ttl).UnixMilli()}
}

func unauthenticated() error {
	return connect.NewError(connect.CodeUnauthenticated, errors.New("expired"))
}

// A credential is renewed 5 minutes before expiry, or halfway through a
// shorter lifetime; the old one serves until the new one arrives.
func TestEnsureRenewal(t *testing.T) {
	now := time.Unix(1000, 0)
	m := credential.New(config.Computer{}, noJitter)
	usable, issue := m.Ensure(now, false)
	assert.False(t, usable)
	require.True(t, issue)
	_, issue = m.Ensure(now, false)
	assert.False(t, issue, "one issue in flight")
	require.NoError(t, m.Issued(now, valid(now, 4*time.Minute, "short"), nil))
	usable, issue = m.Ensure(now.Add(119*time.Second), false)
	assert.True(t, usable)
	assert.False(t, issue, "renewed too early")
	usable, issue = m.Ensure(now.Add(2*time.Minute), false)
	assert.True(t, usable, "old credential keeps serving")
	assert.True(t, issue, "not renewed halfway")
	usable, _ = m.Ensure(now.Add(4*time.Minute), false)
	assert.False(t, usable, "expired credential used")

	// A restored credential of unknown age is renewed 5 minutes before expiry.
	m = credential.New(valid(now, time.Hour, "cached"), noJitter)
	_, issue = m.Ensure(now.Add(54*time.Minute), false)
	assert.False(t, issue)
	_, issue = m.Ensure(now.Add(55*time.Minute), false)
	assert.True(t, issue)
	_, issue = credential.New(config.Computer{}, noJitter).Ensure(now, true)
	assert.False(t, issue, "issue while stopping")
}

// Failed issues back off 30 s · 2^(n−1), or longer if Retry-After says so.
func TestIssueBackoff(t *testing.T) {
	now := time.Unix(1000, 0)
	m := credential.New(config.Computer{}, noJitter)
	down := connect.NewError(connect.CodeUnavailable, errors.New("down"))
	for _, wait := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		_, issue := m.Ensure(now, false)
		require.True(t, issue, wait)
		require.NoError(t, m.Issued(now, config.Computer{}, down))
		_, issue = m.Ensure(now.Add(wait-time.Second), false)
		assert.False(t, issue, "retried before %v", wait)
		now = now.Add(wait)
	}
	slow := connect.NewError(connect.CodeUnavailable, errors.New("slow down"))
	slow.Meta().Set("Retry-After", "600")
	m = credential.New(config.Computer{}, noJitter)
	m.Ensure(now, false)
	require.NoError(t, m.Issued(now, config.Computer{}, slow))
	_, issue := m.Ensure(now.Add(599*time.Second), false)
	assert.False(t, issue, "Retry-After ignored")
}

// Without a session or a registration Run must end.
func TestIssueFatal(t *testing.T) {
	removed := connect.NewError(connect.CodeFailedPrecondition, errors.New("removed"))
	detail, err := connect.NewErrorDetail(&anypb.Any{TypeUrl: "type.googleapis.com/aiserver.v1.GrokBotUserComputerRemovedDetail"})
	require.NoError(t, err)
	removed.AddDetail(detail)
	for _, fatal := range []error{removed, auth.ErrLogout, auth.ErrNotLoggedIn} {
		m := credential.New(config.Computer{}, noJitter)
		m.Ensure(time.Unix(0, 0), false)
		require.ErrorIs(t, m.Issued(time.Unix(0, 0), config.Computer{}, fatal), fatal)
		assert.False(t, m.Issuing())
	}
	assert.True(t, credential.Removed(removed))
	generic := connect.NewError(connect.CodeFailedPrecondition, errors.New("temporary registration state"))
	assert.False(t, credential.Removed(generic), "generic precondition treated as removed")
}

// A rejected credential is dropped. Before it connected, the rejection counts
// as a failed issue; afterwards the next issue starts at once. A rejection of
// a credential that was since replaced is ignored, and so are other errors.
func TestReject(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, proven := range []bool{false, true} {
		m := credential.New(config.Computer{}, noJitter)
		m.Ensure(now, false)
		require.NoError(t, m.Issued(now, valid(now, time.Hour, "c"), nil))
		if proven {
			m.Connected()
		}
		assert.False(t, m.Reject(now, "old", unauthenticated()), "stale rejection")
		assert.False(t, m.Reject(now, "", errors.New("network")), "not a rejection")
		require.True(t, m.Reject(now, "c", unauthenticated()))
		assert.Empty(t, m.Current().Credential)
		_, issue := m.Ensure(now, false)
		assert.Equal(t, proven, issue, "proven=%v", proven)
		if !proven {
			_, issue = m.Ensure(now.Add(30*time.Second), false)
			assert.True(t, issue)
		}
	}
}
