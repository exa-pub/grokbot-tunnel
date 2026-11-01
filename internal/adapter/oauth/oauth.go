// Package oauth talks to the login endpoints of the backend: the PKCE browser
// login (/auth/poll) and the access-token refresh (/oauth/token).
package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/uuid"
)

// ClientID is the OAuth client used for token refresh.
const ClientID = "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB"

const (
	maxBody      = 1 << 20
	pollMaxDelay = 10 * time.Second
	// pollMaxOutage bounds how long login polling rides out network errors,
	// rate limiting and server errors in a row.
	pollMaxOutage = 2 * time.Minute
)

// Variables so tests can shorten them.
var (
	pollTimeout    = 20 * time.Minute
	pollFirstDelay = time.Second
)

// Login is a pending browser login.
type Login struct {
	UUID     string
	Verifier string // PKCE verifier, kept secret until polling
	URL      string // page the user opens to sign in
}

// NewLogin starts a login on the given website.
func NewLogin(website string) Login {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	challenge := sha256.Sum256([]byte(verifier))
	id := uuid.New()
	q := url.Values{
		"challenge":                 {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"uuid":                      {id},
		"mode":                      {"login"},
		"redirectTarget":            {"sand"},
		"supportsSelectedTeamLogin": {"true"},
	}
	return Login{
		UUID:     id,
		Verifier: verifier,
		URL:      strings.TrimRight(website, "/") + "/loginDeepControl?" + q.Encode(),
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Poll waits until the user completes login in the browser. Network errors,
// rate limiting and server errors are retried for up to pollMaxOutage in a
// row; other unexpected responses end the login.
func Poll(ctx context.Context, h *http.Client, base string, l Login) (entity.Identity, error) {
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	q := url.Values{"uuid": {l.UUID}, "verifier": {l.Verifier}}
	endpoint := base + "/auth/poll?" + q.Encode()
	delay := pollFirstDelay
	var outage time.Time // start of the current run of transient failures
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return entity.Identity{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		status, body, err := do(h, req)
		switch {
		case transient(status, err):
			if outage.IsZero() {
				outage = time.Now()
			} else if time.Since(outage) > pollMaxOutage && ctx.Err() == nil {
				if err == nil {
					err = fmt.Errorf("HTTP %d", status)
				}
				return entity.Identity{}, fmt.Errorf("login polling failed: %w", err)
			}
		case status == http.StatusOK:
			return parseLogin(body)
		case status == http.StatusNotFound: // not signed in yet
			outage = time.Time{}
		case status == http.StatusForbidden:
			return entity.Identity{}, errors.New("sign-in policy refused login")
		default:
			return entity.Identity{}, fmt.Errorf("login polling failed: HTTP %d", status)
		}
		if err := sleep(ctx, delay); err != nil {
			// Our own deadline, not the caller's, means the user never finished.
			if parent.Err() == nil {
				err = errors.New("login polling timed out")
			}
			return entity.Identity{}, err
		}
		delay = min(delay*6/5, pollMaxDelay)
	}
}

// transient reports whether a failed poll is worth retrying.
func transient(status int, err error) bool {
	return err != nil || status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

func do(h *http.Client, req *http.Request) (int, []byte, error) {
	r, err := h.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	return r.StatusCode, b, err
}

func parseLogin(body []byte) (entity.Identity, error) {
	var v struct {
		AccessToken    string      `json:"accessToken"`
		RefreshToken   string      `json:"refreshToken"`
		AuthID         string      `json:"authId"`
		SelectedTeamID json.Number `json:"selectedTeamId"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return entity.Identity{}, err
	}
	if v.AccessToken == "" || v.RefreshToken == "" {
		return entity.Identity{}, errors.New("login returned empty tokens")
	}
	return entity.Identity{
		AccessToken:  v.AccessToken,
		RefreshToken: v.RefreshToken,
		AuthID:       v.AuthID,
		TeamID:       string(v.SelectedTeamID),
	}, nil
}

// Refresh exchanges a refresh token for new tokens. The refresh token in the
// result is empty if the server did not rotate it.
func Refresh(ctx context.Context, h *http.Client, base, refreshToken string) (entity.Identity, error) {
	b, err := json.Marshal(map[string]string{
		"client_id":     ClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	if err != nil {
		return entity.Identity{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/oauth/token", bytes.NewReader(b))
	if err != nil {
		return entity.Identity{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	status, body, err := do(h, req)
	if err != nil {
		return entity.Identity{}, err
	}
	var v struct {
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"` // seconds; optional
		ShouldLogout bool        `json:"shouldLogout"`
	}
	decodeErr := json.Unmarshal(body, &v)
	switch {
	case v.ShouldLogout:
		return entity.Identity{}, entity.ErrLogout
	case status != http.StatusOK:
		return entity.Identity{}, fmt.Errorf("refresh failed: HTTP %d", status)
	case decodeErr != nil:
		return entity.Identity{}, fmt.Errorf("refresh failed: %w", decodeErr)
	case v.AccessToken == "":
		return entity.Identity{}, errors.New("refresh failed: empty access token")
	}
	next := entity.Identity{AccessToken: v.AccessToken, RefreshToken: v.RefreshToken}
	if secs, err := v.ExpiresIn.Float64(); err == nil && secs > 0 {
		next.ExpiresAtMS = time.Now().Add(time.Duration(secs * float64(time.Second))).UnixMilli()
	}
	return next, nil
}
