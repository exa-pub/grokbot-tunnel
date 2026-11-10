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
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/uuid"
)

// OAuth clients used for token refresh. Development backends have their own,
// as in the desktop's zh.
const (
	ClientID    = "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB"
	DevClientID = "OzaBXLClY5CAGxNzUhQ2vlknpi07tGuE"
)

// RefreshUserAgent is the User-Agent of token refreshes. The desktop sends
// them with the global fetch of Electron's Node rather than the bundled
// undici that polls the login.
const RefreshUserAgent = "node"

// ClientIDFor returns the OAuth client for the backend at base: override if
// set (SAND_AUTH_CLIENT_ID), DevClientID for loopback, *.lclhst.build and
// dev-staging.cursor.sh, else ClientID.
func ClientIDFor(base, override string) string {
	if override != "" {
		return override
	}
	u, err := url.Parse(base)
	if err != nil {
		return ClientID
	}
	switch host := strings.ToLower(u.Hostname()); {
	case host == "localhost", host == "127.0.0.1", host == "::1",
		strings.HasSuffix(host, ".lclhst.build"), host == "dev-staging.cursor.sh":
		return DevClientID
	}
	return ClientID
}

// Login polling follows the desktop's Ddn: at most pollAttempts polls,
// pollFirstDelay*1.2^n but at most pollMaxDelay apart, and pollMaxFailures
// failed polls in a row end the login.
const (
	maxBody         = 1 << 20
	pollMaxDelay    = 10 * time.Second
	pollMaxFailures = 3
)

// Variables so tests can shorten them.
var (
	pollAttempts   = 150
	pollFirstDelay = time.Second
)

// ErrSignInPolicy is the desktop's SignInPolicyViolationError.
var ErrSignInPolicy = errors.New("sign-in on this device is restricted by your organization's device policy; " +
	"sign in with an allowed account to continue")

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

// Poll waits until the user completes login in the browser, like the
// desktop's Ddn: "not signed in yet" (404) is polled again, and
// pollMaxFailures other failures in a row, network errors included, end the
// login. A 403 ends it at once only for a sign-in policy violation. Errors
// never contain the poll URL, whose query holds the PKCE verifier.
func Poll(ctx context.Context, h *http.Client, base string, l Login) (entity.Identity, error) {
	q := url.Values{"uuid": {l.UUID}, "verifier": {l.Verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/auth/poll?"+q.Encode(), nil)
	if err != nil {
		return entity.Identity{}, fmt.Errorf("login polling: %w", withoutURL(err))
	}
	req.Header.Set("Content-Type", "application/json")
	delay := pollFirstDelay
	failures := 0
	for range pollAttempts {
		status, body, err := do(h, req)
		switch {
		case ctx.Err() != nil:
			return entity.Identity{}, ctx.Err()
		case err != nil:
			err = withoutURL(err)
		case status == http.StatusNotFound: // not signed in yet
			failures = 0
		case status == http.StatusForbidden && policyViolation(body):
			return entity.Identity{}, ErrSignInPolicy
		case status >= 200 && status < 300:
			return parseLogin(body)
		default:
			err = fmt.Errorf("HTTP %d", status)
		}
		if err != nil {
			if failures++; failures >= pollMaxFailures {
				return entity.Identity{}, fmt.Errorf("login polling failed: %w", err)
			}
		}
		if err := sleep(ctx, delay); err != nil {
			return entity.Identity{}, err
		}
		delay = min(delay*6/5, pollMaxDelay)
	}
	return entity.Identity{}, errors.New("login polling timed out")
}

// policyViolation reports whether a 403 body is the desktop's hVe:
// {"error":"sign_in_policy_violation"}.
func policyViolation(body []byte) bool {
	var v struct {
		Error any `json:"error"`
	}
	return json.Unmarshal(body, &v) == nil && v.Error == "sign_in_policy_violation"
}

// withoutURL drops the URL from a *url.Error, whose query may hold secrets,
// keeping the operation and the cause.
func withoutURL(err error) error {
	var ue *url.Error
	if !errors.As(err, &ue) {
		return err
	}
	return fmt.Errorf("%s: %w", ue.Op, ue.Err)
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
		AccessToken    string          `json:"accessToken"`
		RefreshToken   string          `json:"refreshToken"`
		AuthID         string          `json:"authId"`
		SelectedTeamID json.RawMessage `json:"selectedTeamId"`
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
		TeamID:       teamID(v.SelectedTeamID),
	}, nil
}

// maxSafeInteger is JavaScript's Number.MAX_SAFE_INTEGER.
const maxSafeInteger = 1<<53 - 1

// teamID returns the selected team as the desktop keeps it: only a JSON
// number that is a positive safe integer, else none.
func teamID(raw json.RawMessage) string {
	var n float64
	// A JSON string would decode into n only with the string option.
	if err := json.Unmarshal(raw, &n); err != nil || n != math.Trunc(n) || n <= 0 || n > maxSafeInteger {
		return ""
	}
	return strconv.FormatInt(int64(n), 10)
}

// Refresh exchanges a refresh token for new tokens with the OAuth client
// clientID; empty means ClientIDFor(base, ""). The refresh token in the
// result is empty if the server did not rotate it.
func Refresh(ctx context.Context, h *http.Client, base, clientID, refreshToken string) (entity.Identity, error) {
	if clientID == "" {
		clientID = ClientIDFor(base, "")
	}
	b, err := json.Marshal(map[string]string{
		"client_id":     clientID,
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
	req.Header.Set("User-Agent", RefreshUserAgent)
	status, body, err := do(h, req)
	if err != nil {
		return entity.Identity{}, err
	}
	// Like the desktop's runRefreshAccessToken, only a successful response
	// is read: a failed one is temporary whatever its body says.
	if status < 200 || status >= 300 {
		return entity.Identity{}, fmt.Errorf("refresh failed: HTTP %d", status)
	}
	var v struct {
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"` // seconds; optional
		ShouldLogout bool        `json:"shouldLogout"`
	}
	switch err := json.Unmarshal(body, &v); {
	case err != nil:
		return entity.Identity{}, fmt.Errorf("refresh failed: %w", err)
	case v.ShouldLogout:
		return entity.Identity{}, entity.ErrLogout
	case v.AccessToken == "":
		return entity.Identity{}, errors.New("refresh failed: empty access token")
	}
	next := entity.Identity{AccessToken: v.AccessToken, RefreshToken: v.RefreshToken}
	if secs, err := v.ExpiresIn.Float64(); err == nil && secs > 0 {
		next.ExpiresAtMS = time.Now().Add(time.Duration(secs * float64(time.Second))).UnixMilli()
	}
	return next, nil
}
