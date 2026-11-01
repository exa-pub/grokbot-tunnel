// Package tokens hands out the user's access tokens, refreshing them before
// they expire. Several processes share the saved tokens and the server rotates
// the refresh token on every refresh, so refreshes are serialized across
// processes by the store lock.
package tokens

import (
	"context"
	"errors"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/oauth"
	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/staterepo"
)

const (
	// refreshMargin refreshes access tokens shortly before they expire.
	refreshMargin = 5 * time.Minute
	// rejectCooldown is how long a rejected token does not trigger another
	// refresh, as in the desktop's unauthenticated recovery.
	rejectCooldown = 5 * time.Minute
)

// TokenSource hands out access tokens, refreshing and persisting them as
// needed. Concurrent callers share a single refresh.
type TokenSource struct {
	mu       sync.Mutex
	Store    *staterepo.Store
	HTTP     *http.Client
	Base     string
	ClientID string // OAuth client; empty means oauth.ClientIDFor(Base, "")
	Identity entity.Identity

	rejectedAt time.Time // when Rejected last refreshed
}

// fresh reports whether the access token can be used without a refresh. A
// token of unknown lifetime is used until the server rejects it, and
// Rejected then replaces it.
func fresh(id entity.Identity) bool {
	if id.AccessToken == "" {
		return false
	}
	exp := id.Expiry()
	return exp.IsZero() || time.Until(exp) > refreshMargin
}

// sameAccount reports whether two identities may belong to the same account
// and team. Identities without a known account are assumed to match.
func sameAccount(a, b entity.Identity) bool {
	sa, sb := a.AccountScope(), b.AccountScope()
	return sa == "" || sb == "" || sa == sb
}

// Token returns a valid access token. force refreshes even if the current
// token looks valid, e.g. after the server rejected it.
//
// Several processes share auth.json and the server rotates the refresh token
// on every refresh, so refreshes are serialized by the store lock and start
// from the tokens on disk: if another process already rotated them, its
// tokens are adopted instead of refreshing with the stale refresh token, which
// the server would answer with shouldLogout.
func (s *TokenSource) Token(ctx context.Context, force bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !force && fresh(s.Identity) {
		return s.Identity.AccessToken, nil
	}
	return s.refresh(ctx)
}

// Rejected refreshes the access token after the server refused token, like
// the desktop's unauthenticated recovery (Hz): only if token is still the
// current one, so that callers that failed together refresh once, and at
// most once per rejectCooldown, so that a server that keeps refusing is not
// answered with a refresh each time.
func (s *TokenSource) Rejected(ctx context.Context, token string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if token == "" || token != s.Identity.AccessToken || time.Since(s.rejectedAt) < rejectCooldown {
		return nil
	}
	s.rejectedAt = time.Now()
	_, err := s.refresh(ctx)
	return err
}

// Snapshot returns the identity without racing token rotation.
func (s *TokenSource) Snapshot() entity.Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Identity
}

// refresh refreshes the access token. The caller holds s.mu.
func (s *TokenSource) refresh(ctx context.Context) (string, error) {
	id := s.Identity
	unlock, err := s.lock(ctx)
	if err != nil {
		return "", err
	}
	defer unlock()
	onDisk, persist := s.reload()
	if s.Identity.RefreshToken != id.RefreshToken {
		id = s.Identity
		// The adopted access token is not the one the server rejected.
		if fresh(id) {
			return id.AccessToken, nil
		}
	}
	if id.RefreshToken == "" {
		return "", entity.ErrNotLoggedIn
	}
	next, err := oauth.Refresh(ctx, s.HTTP, s.Base, s.ClientID, id.RefreshToken)
	if errors.Is(err, entity.ErrLogout) {
		s.Identity = entity.Identity{}
		// Only wipe auth.json if it still holds the rejected session.
		if onDisk == id.RefreshToken {
			if err := s.Store.Save("auth", entity.Identity{}); err != nil {
				return "", err
			}
		}
		return "", err
	}
	if err != nil {
		return "", err
	}
	id.AccessToken, id.ExpiresAtMS = next.AccessToken, next.ExpiresAtMS
	if next.RefreshToken != "" {
		id.RefreshToken = next.RefreshToken
	}
	// The server already invalidated the old refresh token, so keep the new
	// one in memory even if it cannot be persisted.
	s.Identity = id
	if persist {
		if err := s.Store.Save("auth", id); err != nil {
			log.Printf("save refreshed tokens: %v", err)
		}
	}
	return id.AccessToken, nil
}

// lock takes the store lock, failing only if ctx ends. An unwritable store
// must not stop the refresh itself, so other errors leave it unlocked.
func (s *TokenSource) lock(ctx context.Context) (func(), error) {
	unlock, err := s.Store.Lock(ctx)
	switch {
	case err == nil:
		return unlock, nil
	case ctx.Err() != nil:
		return nil, ctx.Err()
	}
	log.Printf("lock auth store: %v", err)
	return func() {}, nil
}

// reload adopts the tokens on disk if another process rotated them since this
// one loaded them. It returns the refresh token on disk, empty if none, and
// whether this process may save its tokens there: not if the disk belongs to a
// different account, which means the user logged in again elsewhere. This
// process then keeps using its own tokens but leaves the new login alone.
// The caller holds the store lock.
func (s *TokenSource) reload() (onDisk string, persist bool) {
	var disk entity.Identity
	if err := s.Store.Load("auth", &disk); err != nil || disk.RefreshToken == "" {
		return "", true
	}
	if !sameAccount(s.Identity, disk) {
		return disk.RefreshToken, false
	}
	if disk.RefreshToken != s.Identity.RefreshToken {
		s.Identity = disk
	}
	return disk.RefreshToken, true
}
