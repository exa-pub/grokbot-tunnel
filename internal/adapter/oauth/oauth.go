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
	"unicode"
	"unicode/utf16"

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
	// Key binds the tokens of the login, and ShortCode is the code the
	// sign-in page shows for it; both are set by RegisterIntent.
	Key       *DPoPKey
	ShortCode string

	challenge string
}

// NewLogin starts a login on the given website.
func NewLogin(website string) Login {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	id := uuid.New()
	q := url.Values{
		"challenge":                 {challenge},
		"uuid":                      {id},
		"mode":                      {"login"},
		"redirectTarget":            {"sand"},
		"supportsSelectedTeamLogin": {"true"},
	}
	return Login{
		UUID:      id,
		Verifier:  verifier,
		URL:       strings.TrimRight(website, "/") + "/loginDeepControl?" + q.Encode(),
		challenge: challenge,
	}
}

// Device names in a login intent are at most this long, in UTF-16 units.
const deviceNameMax = 120

// DeviceName is the desktop's name of this device in a login intent: the
// host name without control and format characters, trimmed and cut to
// deviceNameMax, or "Cursor CLI".
func DeviceName(hostname string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cc, unicode.Cf) {
			return -1
		}
		return r
	}, hostname)
	name := entity.TrimSpace(cleaned)
	units := 0
	cut := strings.IndexFunc(name, func(r rune) bool {
		units += utf16.RuneLen(r)
		return units > deviceNameMax
	})
	if cut >= 0 {
		name = entity.TrimSpace(name[:cut])
	}
	if name == "" {
		return "Cursor CLI"
	}
	return name
}

// RegisterIntent binds the login to a new DPoP key before the user opens
// its page, as the desktop does since 1.3 (nX, /auth/login_intent). On
// success l.Key is set and the login is polled with proofs of it.
func RegisterIntent(ctx context.Context, h *http.Client, base string, l *Login, deviceName string) error {
	key, err := NewDPoPKey()
	if err != nil {
		return err
	}
	b, err := json.Marshal(struct {
		UUID       string `json:"uuid"`
		Challenge  string `json:"challenge"`
		DeviceName string `json:"deviceName"`
		Platform   string `json:"platform"`
		DPoPJkt    string `json:"dpop_jkt"`
	}{l.UUID, l.challenge, deviceName, "linux", key.Thumbprint()})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/auth/login_intent", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	r, err := send(h, req)
	switch {
	case err != nil:
		return fmt.Errorf("login intent: %w", err)
	case r.status < 200 || r.status >= 300:
		return fmt.Errorf("login intent: %w", errHTTP(r.status))
	}
	var v struct {
		ShortCode *string `json:"shortCode"`
	}
	if json.Unmarshal(r.body, &v) != nil || v.ShortCode == nil {
		return errors.New("login_intent response did not include shortCode")
	}
	l.Key, l.ShortCode = key, *v.ShortCode
	return nil
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
// never contain the poll URL, whose query holds the PKCE verifier. A login
// with a key is polled by POST with a proof (zve, IL).
func Poll(ctx context.Context, h *http.Client, base string, l Login) (entity.Identity, error) {
	var clock serverClock
	delay := pollFirstDelay
	failures := 0
	for range pollAttempts {
		r, err := pollOnce(ctx, h, base, l, &clock)
		switch {
		case ctx.Err() != nil:
			return entity.Identity{}, ctx.Err()
		case err != nil:
			err = withoutURL(err)
		case r.status == http.StatusNotFound: // not signed in yet
			failures = 0
		case r.status == http.StatusForbidden && policyViolation(r.body):
			return entity.Identity{}, ErrSignInPolicy
		case r.status >= 200 && r.status < 300:
			return parseLogin(r.body, l.Key)
		default:
			err = errHTTP(r.status)
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

// pollOnce polls the login once. With a key, a proof the server refused
// for the time is sent again once the answer moved the clock (IL).
func pollOnce(ctx context.Context, h *http.Client, base string, l Login, clock *serverClock) (response, error) {
	r, err := pollRequest(ctx, h, base, l, clock)
	if err != nil || l.Key == nil {
		return r, err
	}
	// The clock follows every answer; a refusal of the proof for the
	// time is retried if it moved.
	if !clock.shifted(r.header) || r.status != http.StatusUnauthorized || !invalidProof(r.body) {
		return r, nil
	}
	r, err = pollRequest(ctx, h, base, l, clock)
	if err == nil {
		clock.observe(r.header)
	}
	return r, err
}

func pollRequest(ctx context.Context, h *http.Client, base string, l Login, clock *serverClock) (response, error) {
	target := base + "/auth/poll"
	var req *http.Request
	var err error
	if l.Key == nil {
		q := url.Values{"uuid": {l.UUID}, "verifier": {l.Verifier}}
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, target+"?"+q.Encode(), nil)
	} else {
		var b []byte
		b, err = json.Marshal(struct {
			UUID     string `json:"uuid"`
			Verifier string `json:"verifier"`
		}{l.UUID, l.Verifier})
		if err != nil {
			return response{}, err
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(b))
	}
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if l.Key != nil {
		proof, err := l.Key.Proof(http.MethodPost, target, clock.now())
		if err != nil {
			return response{}, err
		}
		req.Header.Set(dpopHeader, proof)
	}
	return send(h, req)
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

func limitReader(r *http.Response) io.Reader { return io.LimitReader(r.Body, maxBody) }

// parseLogin reads the tokens of a login (lUt). Tokens the server bound to
// key, by a token type of DPoP or the form of the refresh token, keep it
// with the refresh token.
func parseLogin(body []byte, key *DPoPKey) (entity.Identity, error) {
	var v struct {
		AccessToken    string          `json:"accessToken"`
		RefreshToken   string          `json:"refreshToken"`
		TokenTypeSnake any             `json:"token_type"`
		TokenType      any             `json:"tokenType"`
		AuthID         string          `json:"authId"`
		SelectedTeamID json.RawMessage `json:"selectedTeamId"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return entity.Identity{}, err
	}
	if v.AccessToken == "" || v.RefreshToken == "" {
		return entity.Identity{}, errors.New("login returned empty tokens")
	}
	tokenType := v.TokenTypeSnake // token_type ?? tokenType
	if tokenType == nil {
		tokenType = v.TokenType
	}
	refresh := v.RefreshToken
	if tokenType == "DPoP" || strings.HasPrefix(refresh, boundRefreshPrefix) {
		refresh = wrapRefresh(refresh, key)
	}
	return entity.Identity{
		AccessToken:  v.AccessToken,
		RefreshToken: refresh,
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
// result is empty if the server did not rotate it. A refresh token bound to
// a DPoP key (dpop:v1:) is sent with a proof, and a rotated one stays bound
// to the same key (vwe); one whose key cannot be read ends the session, as
// the desktop revokes it.
func Refresh(ctx context.Context, h *http.Client, base, clientID, refreshToken string) (entity.Identity, error) {
	if clientID == "" {
		clientID = ClientIDFor(base, "")
	}
	token, key, err := unwrapRefresh(refreshToken)
	if err != nil {
		return entity.Identity{}, fmt.Errorf("%w: %w", entity.ErrLogout, err)
	}
	b, err := json.Marshal(map[string]string{
		"client_id":     clientID,
		"grant_type":    "refresh_token",
		"refresh_token": token,
	})
	if err != nil {
		return entity.Identity{}, err
	}
	r, err := refreshRequest(ctx, h, base, b, key)
	// A proof the server refused for the time is sent once more (Ewe).
	if err == nil && key != nil && ctx.Err() == nil && retryProof(&refreshClock, r.status, r.header, r.body) {
		r, err = refreshRequest(ctx, h, base, b, key)
	}
	if err != nil {
		return entity.Identity{}, err
	}
	return parseRefresh(r, key)
}

// parseRefresh reads the answer of a refresh. Like the desktop's
// runRefreshAccessToken, only a successful one is read: a failed one is
// temporary whatever its body says.
func parseRefresh(r response, key *DPoPKey) (entity.Identity, error) {
	if r.status < 200 || r.status >= 300 {
		return entity.Identity{}, fmt.Errorf("refresh failed: %w", errHTTP(r.status))
	}
	var v struct {
		AccessToken  string      `json:"access_token"`
		RefreshToken string      `json:"refresh_token"`
		ExpiresIn    json.Number `json:"expires_in"` // seconds; optional
		ShouldLogout bool        `json:"shouldLogout"`
	}
	switch err := json.Unmarshal(r.body, &v); {
	case err != nil:
		return entity.Identity{}, fmt.Errorf("refresh failed: %w", err)
	case v.ShouldLogout:
		return entity.Identity{}, entity.ErrLogout
	case v.AccessToken == "":
		return entity.Identity{}, errors.New("refresh failed: empty access token")
	}
	next := entity.Identity{AccessToken: v.AccessToken}
	if v.RefreshToken != "" {
		next.RefreshToken = wrapRefresh(v.RefreshToken, key)
	}
	if secs, err := v.ExpiresIn.Float64(); err == nil && secs > 0 {
		next.ExpiresAtMS = time.Now().Add(time.Duration(secs * float64(time.Second))).UnixMilli()
	}
	return next, nil
}

func refreshRequest(ctx context.Context, h *http.Client, base string, body []byte, key *DPoPKey) (response, error) {
	target := base + "/oauth/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", RefreshUserAgent)
	if key != nil {
		proof, err := key.Proof(http.MethodPost, target, refreshClock.now())
		if err != nil {
			return response{}, err
		}
		req.Header.Set(dpopHeader, proof)
	}
	return send(h, req)
}
