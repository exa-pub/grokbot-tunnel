// Package credential keeps the computer credential of the user-computer
// channel, like the desktop's credential issuer and coordinator: renewal
// before expiry, retries with backoff, and dropping a credential the server
// rejects. Manager is a synchronous state machine; the caller runs the issue
// RPC and caches the credential.
package credential

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
)

const (
	// Margin is how long before expiry a credential is renewed.
	Margin = 5 * time.Minute
	// Failed issues are retried like the desktop's gJe: 30 s · 2^(n−1) up to
	// 10 min, or Retry-After up to 15 min, plus jitter.
	issueBackoff    = 30 * time.Second
	maxIssueBackoff = 10 * time.Minute
	maxRetryAfter   = 15 * time.Minute
)

// Manager holds the current credential.
type Manager struct {
	jitter func(time.Duration) time.Duration
	c      entity.Credential
	// issuedAt is the local time the credential was issued; zero if unknown.
	issuedAt time.Time
	// proven is set once the watch stream connected with the credential, as in
	// the desktop; until then a rejection counts as a failed issue.
	proven bool
	// Failed issues, and credentials rejected before they were proven, are
	// retried after nextRenew.
	issuing   bool
	failures  int
	nextRenew time.Time
}

// New returns a Manager starting with cached, which may be empty; jitter
// returns the random part of a retry delay d.
func New(cached entity.Credential, jitter func(d time.Duration) time.Duration) *Manager {
	m := &Manager{jitter: jitter}
	m.Restore(cached)
	return m
}

// Restore replaces the credential with one of unknown age, as loaded from
// the cache.
func (m *Manager) Restore(c entity.Credential) {
	m.c, m.issuedAt, m.proven = c, time.Time{}, false
}

// Current returns the credential; its Credential is empty without one.
func (m *Manager) Current() entity.Credential { return m.c }

// Issuing reports whether an issue is in flight.
func (m *Manager) Issuing() bool { return m.issuing }

// Ensure reports whether a usable credential is available, and whether the
// caller should issue a new one now, which it then marks in flight. While an
// issue is in flight or backing off, the old credential keeps serving until
// it expires. stopping suppresses new issues.
func (m *Manager) Ensure(now time.Time, stopping bool) (usable, issue bool) {
	if m.c.Credential != "" && now.Before(m.renewAt()) {
		return true, false
	}
	usable = m.c.Credential != "" && now.Before(time.UnixMilli(m.c.ExpiresAtMS))
	if !m.issuing && !stopping && !now.Before(m.nextRenew) {
		m.issuing = true
		issue = true
	}
	return usable, issue
}

// Issued applies the result of the issue in flight. Without a session or a
// registration no credential can ever be issued, so those errors are
// returned as fatal; other failures are retried with backoff.
func (m *Manager) Issued(now time.Time, c entity.Credential, err error) (fatal error) {
	m.issuing = false
	switch {
	case Removed(err):
		return fmt.Errorf("computer registration refused: %w", err)
	case errors.Is(err, entity.ErrLogout), errors.Is(err, entity.ErrNotLoggedIn):
		return err
	case err != nil:
		m.fail(now, retryAfter(err))
		return nil
	}
	m.c, m.issuedAt, m.proven = c, now, false
	m.nextRenew = time.Time{}
	return nil
}

// Connected records that the server accepted the credential on the watch
// stream.
func (m *Manager) Connected() {
	m.proven = true
	m.failures = 0
}

// Reject drops the credential if err shows that the server rejected it, and
// reports whether it did. A result for credential, if not empty, says nothing
// about a credential that has since replaced it. A credential rejected before
// it ever connected counts as a failed issue, so a server that keeps
// rejecting it is not hammered.
func (m *Manager) Reject(now time.Time, credential string, err error) bool {
	if credential != "" && credential != m.c.Credential || connect.CodeOf(err) != connect.CodeUnauthenticated {
		return false
	}
	if !m.proven {
		m.fail(now, retryAfter(err))
	}
	m.Restore(entity.Credential{})
	return true
}

// fail schedules the next issue after a failure, like the desktop's gJe.
func (m *Manager) fail(now time.Time, retryAfter time.Duration) {
	m.failures++
	d := min(maxIssueBackoff, issueBackoff<<min(m.failures-1, 10))
	d = max(d, min(retryAfter, maxRetryAfter))
	m.nextRenew = now.Add(d + m.jitter(d/2))
}

// retryAfter returns the delay the server asked for in a Retry-After header
// of err, in seconds; zero if none.
func retryAfter(err error) time.Duration {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return 0
	}
	n, perr := strconv.Atoi(ce.Meta().Get("Retry-After"))
	if perr != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// Removed reports whether err says the computer's registration was removed.
func Removed(err error) bool {
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		return false
	}
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return false
	}
	for _, d := range ce.Details() {
		if d.Type() == "aiserver.v1.GrokBotUserComputerRemovedDetail" {
			return true
		}
	}
	return false
}

// renewAt returns when the credential should be renewed: Margin before it
// expires, or halfway through its lifetime if that is shorter.
func (m *Manager) renewAt() time.Time {
	expiresAt := time.UnixMilli(m.c.ExpiresAtMS)
	margin := Margin
	if !m.issuedAt.IsZero() {
		margin = min(margin, expiresAt.Sub(m.issuedAt)/2)
	}
	return expiresAt.Add(-margin)
}
