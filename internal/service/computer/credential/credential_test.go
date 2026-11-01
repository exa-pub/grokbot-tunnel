package credential_test

import (
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/credential"
)

func noJitter(time.Duration) time.Duration { return 0 }

func valid(now time.Time, ttl time.Duration, name string) entity.Credential {
	return entity.Credential{Credential: name, ExpiresAtMS: now.Add(ttl).UnixMilli()}
}

func unauthenticated() error {
	return connect.NewError(connect.CodeUnauthenticated, errors.New("expired"))
}

// A credential is renewed 5 minutes before expiry, or halfway through a
// shorter lifetime; the old one serves until the new one arrives.
func TestEnsureRenewal(t *testing.T) {
	now := time.Unix(1000, 0)
	m := credential.New(entity.Credential{}, noJitter)
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
	_, issue = credential.New(entity.Credential{}, noJitter).Ensure(now, true)
	assert.False(t, issue, "issue while stopping")
}

// Failed issues back off 30 s · 2^(n−1), or longer if Retry-After says so.
func TestIssueBackoff(t *testing.T) {
	now := time.Unix(1000, 0)
	m := credential.New(entity.Credential{}, noJitter)
	down := connect.NewError(connect.CodeUnavailable, errors.New("down"))
	for _, wait := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		_, issue := m.Ensure(now, false)
		require.True(t, issue, wait)
		require.NoError(t, m.Issued(now, entity.Credential{}, down))
		_, issue = m.Ensure(now.Add(wait-time.Second), false)
		assert.False(t, issue, "retried before %v", wait)
		now = now.Add(wait)
	}
	slow := connect.NewError(connect.CodeUnavailable, errors.New("slow down"))
	slow.Meta().Set("Retry-After", "600")
	m = credential.New(entity.Credential{}, noJitter)
	m.Ensure(now, false)
	require.NoError(t, m.Issued(now, entity.Credential{}, slow))
	_, issue := m.Ensure(now.Add(599*time.Second), false)
	assert.False(t, issue, "Retry-After ignored")
}

// Without a session Run must end.
func TestIssueFatal(t *testing.T) {
	for _, fatal := range []error{entity.ErrLogout, entity.ErrNotLoggedIn} {
		m := credential.New(entity.Credential{}, noJitter)
		m.Ensure(time.Unix(0, 0), false)
		require.ErrorIs(t, m.Issued(time.Unix(0, 0), entity.Credential{}, fatal), fatal)
		assert.False(t, m.Issuing())
	}
	removed := removedError(t)
	assert.True(t, credential.Removed(removed))
	generic := connect.NewError(connect.CodeFailedPrecondition, errors.New("temporary registration state"))
	assert.False(t, credential.Removed(generic), "generic precondition treated as removed")
}

func removedError(t *testing.T) error {
	t.Helper()
	removed := connect.NewError(connect.CodeFailedPrecondition, errors.New("removed"))
	detail, err := connect.NewErrorDetail(&anypb.Any{TypeUrl: "type.googleapis.com/aiserver.v1.GrokBotUserComputerRemovedDetail"})
	require.NoError(t, err)
	removed.AddDetail(detail)
	return removed
}

// A removal ends Run only once this process held a credential, like the
// desktop's ket (scopes.held); before that, and with only a cached
// credential, it is retried with the issue backoff.
func TestIssueRemoved(t *testing.T) {
	now := time.Unix(1000, 0)
	removed := removedError(t)
	for _, tc := range []struct {
		name   string
		cached entity.Credential
		issued bool // an issue succeeded first
		fatal  bool
	}{
		{"first issue", entity.Credential{}, false, false},
		{"cached credential", valid(now, time.Hour, "cached"), false, false},
		{"after an issue", entity.Credential{}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := credential.New(tc.cached, noJitter)
			if tc.issued {
				m.Ensure(now, false)
				require.NoError(t, m.Issued(now, valid(now, 4*time.Minute, "c"), nil))
			}
			at := now.Add(55 * time.Minute) // past the renewal of any credential above
			_, issue := m.Ensure(at, false)
			require.True(t, issue)
			err := m.Issued(at, entity.Credential{}, removed)
			if !tc.fatal {
				require.NoError(t, err)
				_, issue = m.Ensure(at.Add(29*time.Second), false)
				assert.False(t, issue, "removal retried before the backoff")
				_, issue = m.Ensure(at.Add(30*time.Second), false)
				assert.True(t, issue, "removal not retried after the backoff")
				return
			}
			require.ErrorIs(t, err, credential.ErrRemoved)
			require.ErrorIs(t, err, removed)
		})
	}
}

// A rejected credential is dropped. Before it connected, the rejection counts
// as a failed issue; afterwards the next issue starts at once. A rejection of
// a credential that was since replaced is ignored, and so are other errors.
func TestReject(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, proven := range []bool{false, true} {
		m := credential.New(entity.Credential{}, noJitter)
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
