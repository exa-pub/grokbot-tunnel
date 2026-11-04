// Package tokens hands out the user's access tokens, refreshing them before
// they expire. Several processes share the saved tokens and the server rotates
// the refresh token on every refresh, so refreshes are serialized across
// processes by the store lock, and a refresh, once started, is not canceled
// by its callers: the server may already have rotated the refresh token. For
// the same reason a process waits for it before exiting.
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
	// refreshTimeout bounds waiting for the store lock and the refresh
	// request, each, as the desktop's deadline (hAn) bounds its fetch.
	refreshTimeout = 20 * time.Second
	// WaitTimeout bounds Wait at exit: a refresh takes at most refreshTimeout
	// for the lock and again for the request, plus time to save its answer.
	WaitTimeout = 2*refreshTimeout + 5*time.Second
)

// TokenSource hands out access tokens, refreshing and persisting them as
// needed. Concurrent callers share a single refresh, like the desktop's
// refreshFlights. Identity is set before first use; afterwards it is read
// with Snapshot.
type TokenSource struct {
	mu       sync.Mutex // guards Identity, rejectedAt and flight
	Store    *staterepo.Store
	HTTP     *http.Client
	Base     string
	ClientID string // OAuth client; empty means oauth.ClientIDFor(Base, "")
	Identity entity.Identity

	rejectedAt time.Time // when Rejected last refreshed
	flight     *flight   // the refresh in progress, nil if none
}

// flight is a refresh shared by its callers. token and err are set before
// done is closed.
type flight struct {
	done  chan struct{}
	token string
	err   error
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
//
// A refresh in progress is joined. The caller waits for it or for ctx, but
// the refresh does not end with ctx: its answer is saved anyway, as long as
// the process waits for it before exiting (see Wait).
func (s *TokenSource) Token(ctx context.Context, force bool) (string, error) {
	s.mu.Lock()
	if s.flight == nil && !force && fresh(s.Identity) {
		tok := s.Identity.AccessToken
		s.mu.Unlock()
		return tok, nil
	}
	f, err := s.start(ctx)
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	return f.wait(ctx)
}

// Rejected refreshes the access token after the server refused token, like
// the desktop's unauthenticated recovery (Hz): only if token is still the
// current one, so that callers that failed together refresh once, and at
// most once per rejectCooldown, so that a server that keeps refusing is not
// answered with a refresh each time. A refresh in progress is joined
// instead, as in Token.
func (s *TokenSource) Rejected(ctx context.Context, token string) error {
	s.mu.Lock()
	if s.flight == nil {
		if token == "" || token != s.Identity.AccessToken || time.Since(s.rejectedAt) < rejectCooldown {
			s.mu.Unlock()
			return nil
		}
		// A caller that is gone starts no refresh, so it arms no cooldown.
		if err := ctx.Err(); err != nil {
			s.mu.Unlock()
			return err
		}
		s.rejectedAt = time.Now()
	}
	f, err := s.start(ctx)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	_, err = f.wait(ctx)
	return err
}

// Wait waits until no refresh is in progress, or for ctx. A refresh does not
// end with its callers, so a process waits for it before exiting: cut off,
// its answer would be lost after the server may already have rotated the
// refresh token, and the next start would be logged out with shouldLogout.
// The desktop's refresh likewise runs to its own deadline (hAn, A@1271742),
// since a failed registration does not end the app. Callers bound ctx by
// WaitTimeout.
func (s *TokenSource) Wait(ctx context.Context) error {
	for {
		s.mu.Lock()
		f := s.flight
		s.mu.Unlock()
		if f == nil {
			return nil
		}
		select {
		case <-f.done:
			// A caller may have started another refresh meanwhile.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Snapshot returns the identity without racing token rotation.
func (s *TokenSource) Snapshot() entity.Identity {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.Identity
}

// start returns the refresh in progress, starting one if there is none. The
// refresh keeps ctx's values but not its cancellation. A caller whose ctx has
// already ended starts none and gets ctx's error: nobody would wait for the
// refresh, and its process may be exiting, which would cut it off after the
// server rotated the refresh token. The caller holds s.mu.
func (s *TokenSource) start(ctx context.Context) (*flight, error) {
	if s.flight == nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		f := &flight{done: make(chan struct{})}
		s.flight = f
		go s.run(context.WithoutCancel(ctx), f)
	}
	return s.flight, nil
}

// run performs the refresh f and hands its result to f's callers.
func (s *TokenSource) run(ctx context.Context, f *flight) {
	f.token, f.err = s.refresh(ctx)
	s.mu.Lock()
	s.flight = nil
	s.mu.Unlock()
	close(f.done)
}

// wait returns the result of f, or ctx's error if ctx ends first.
func (f *flight) wait(ctx context.Context) (string, error) {
	select {
	case <-f.done:
		return f.token, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// refresh refreshes the access token. It runs only from run, one at a time,
// and takes s.mu only around the identity so that callers can stop waiting.
func (s *TokenSource) refresh(ctx context.Context) (string, error) {
	lockCtx, cancelLock := context.WithTimeout(ctx, refreshTimeout)
	defer cancelLock()
	unlock, err := s.lock(lockCtx)
	if err != nil {
		return "", err
	}
	defer unlock()
	s.mu.Lock()
	id := s.Identity
	onDisk, persist := s.reload()
	adopted := s.Identity.RefreshToken != id.RefreshToken
	id = s.Identity
	s.mu.Unlock()
	// The adopted access token is not the one the server rejected.
	if adopted && fresh(id) {
		return id.AccessToken, nil
	}
	if id.RefreshToken == "" {
		return "", entity.ErrNotLoggedIn
	}
	httpCtx, cancelHTTP := context.WithTimeout(ctx, refreshTimeout)
	defer cancelHTTP()
	next, err := oauth.Refresh(httpCtx, s.HTTP, s.Base, s.ClientID, id.RefreshToken)
	if errors.Is(err, entity.ErrLogout) {
		s.mu.Lock()
		s.Identity = entity.Identity{}
		s.mu.Unlock()
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
	s.mu.Lock()
	s.Identity = id
	s.mu.Unlock()
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
// The caller holds the store lock and s.mu.
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
