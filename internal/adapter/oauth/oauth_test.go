package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
)

func TestPKCE(t *testing.T) {
	l := NewLogin("https://cursor.com")
	u, _ := url.Parse(l.URL)
	h := sha256.Sum256([]byte(l.Verifier))
	if u.Query().Get("challenge") != base64.RawURLEncoding.EncodeToString(h[:]) || len(l.Verifier) != 43 || u.Query().Get("redirectTarget") != "sand" {
		t.Fatal("bad PKCE")
	}
}

func TestPoll(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("verifier") != "v" {
			t.Error("missing verifier")
		}
		fmt.Fprint(w, `{"accessToken":"a","refreshToken":"r","selectedTeamId":12}`)
	}))
	defer s.Close()
	a, e := Poll(context.Background(), s.Client(), s.URL, Login{UUID: "u", Verifier: "v"})
	if e != nil || a.TeamID != "12" || a.AccessToken != "a" {
		t.Fatal(a, e)
	}
}

func TestPollInvalidBaseURL(t *testing.T) {
	const secret = "s3cr3t-verifier"
	_, err := Poll(context.Background(), http.DefaultClient, "http://bad host", Login{UUID: "u", Verifier: secret})
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal(err)
	}
}

// fastPoll shortens the delays between polls.
func fastPoll(t *testing.T) {
	t.Helper()
	old := pollFirstDelay
	pollFirstDelay = time.Millisecond
	t.Cleanup(func() { pollFirstDelay = old })
}

// pollServer answers poll n with statuses[n], 404 once they run out, and
// with body for 403s.
func pollServer(t *testing.T, body string, statuses ...int) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		status := http.StatusNotFound
		if n <= len(statuses) {
			status = statuses[n-1]
		}
		switch status {
		case http.StatusOK:
			fmt.Fprint(w, `{"accessToken":"a","refreshToken":"r"}`)
		case http.StatusForbidden:
			w.WriteHeader(status)
			fmt.Fprint(w, body)
		default:
			w.WriteHeader(status)
		}
	}))
	t.Cleanup(s.Close)
	return s, &calls
}

// Like the desktop's Ddn, 404 resets the count of failed polls and three
// failures in a row end the login, whatever they are.
func TestPollFailuresInARow(t *testing.T) {
	fastPoll(t)
	s, _ := pollServer(t, `{}`, 503, 429, 404, 500, http.StatusForbidden, 404, 200)
	if a, err := Poll(context.Background(), s.Client(), s.URL, Login{UUID: "u", Verifier: "v"}); err != nil || a.AccessToken != "a" {
		t.Fatal(a, err)
	}
	s, calls := pollServer(t, `{}`, 503, 429, 400, 200)
	_, err := Poll(context.Background(), s.Client(), s.URL, Login{UUID: "u", Verifier: "v"})
	if err == nil || err.Error() != "login polling failed: HTTP 400" || calls.Load() != 3 {
		t.Fatal(err, calls.Load())
	}
}

// A 403 ends the login at once only for a sign-in policy violation.
func TestPollSignInPolicy(t *testing.T) {
	fastPoll(t)
	s, calls := pollServer(t, `{"error":"sign_in_policy_violation"}`, http.StatusForbidden, 200)
	if _, err := Poll(context.Background(), s.Client(), s.URL, Login{UUID: "u", Verifier: "v"}); !errors.Is(err, ErrSignInPolicy) || calls.Load() != 1 {
		t.Fatal(err, calls.Load())
	}
	s, _ = pollServer(t, `{"error":"other"}`, http.StatusForbidden, 200)
	if a, err := Poll(context.Background(), s.Client(), s.URL, Login{UUID: "u", Verifier: "v"}); err != nil || a.AccessToken != "a" {
		t.Fatal(a, err)
	}
}

// Network errors count as failed polls, and the error does not contain the
// poll URL with the verifier.
func TestPollNetworkErrorHidesVerifier(t *testing.T) {
	fastPoll(t)
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
			conn.Close()
		}
	}))
	defer s.Close()
	const secret = "s3cr3t-verifier"
	_, err := Poll(context.Background(), s.Client(), s.URL, Login{UUID: "u", Verifier: secret})
	if err == nil || strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "verifier") || calls.Load() != 3 {
		t.Fatal(err, calls.Load())
	}
	if !strings.HasPrefix(err.Error(), "login polling failed: Get: ") {
		t.Fatal(err)
	}
}

func TestPollCanceled(t *testing.T) {
	fastPoll(t)
	s, _ := pollServer(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := Poll(ctx, s.Client(), s.URL, Login{UUID: "u", Verifier: "v"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestPollTimeoutMessage(t *testing.T) {
	fastPoll(t)
	defer func(n int) { pollAttempts = n }(pollAttempts)
	pollAttempts = 5
	s, calls := pollServer(t, "")
	if _, err := Poll(context.Background(), s.Client(), s.URL, Login{UUID: "u", Verifier: "v"}); err == nil ||
		err.Error() != "login polling timed out" || calls.Load() != 5 {
		t.Fatal(err, calls.Load())
	}
}

// The selected team is kept, as in the desktop, only if it is a JSON number
// that is a positive safe integer.
func TestSelectedTeamID(t *testing.T) {
	for raw, want := range map[string]string{
		`12`: "12", `12.0`: "12", `1e3`: "1000", `9007199254740991`: "9007199254740991",
		`0`: "", `-1`: "", `1.5`: "", `9007199254740992`: "", `"12"`: "", `null`: "", `true`: "",
	} {
		id, err := parseLogin([]byte(`{"accessToken":"a","refreshToken":"r","selectedTeamId":` + raw + `}`))
		if err != nil || id.TeamID != want {
			t.Errorf("%s: got %q, %v; want %q", raw, id.TeamID, err, want)
		}
	}
	if id, err := parseLogin([]byte(`{"accessToken":"a","refreshToken":"r"}`)); err != nil || id.TeamID != "" {
		t.Fatal(id, err)
	}
}

func TestClientIDFor(t *testing.T) {
	for base, want := range map[string]string{
		"https://api2.cursor.sh":           ClientID,
		"http://localhost:8000":            DevClientID,
		"http://127.0.0.1:8000":            DevClientID,
		"http://[::1]:8000":                DevClientID,
		"https://API.box.lclhst.build":     DevClientID,
		"https://dev-staging.cursor.sh/":   DevClientID,
		"https://staging.cursor.sh":        ClientID,
		"https://lclhst.build.example.com": ClientID,
		"://bad":                           ClientID,
	} {
		if got := ClientIDFor(base, ""); got != want {
			t.Errorf("%s: got %s, want %s", base, got, want)
		}
	}
	if got := ClientIDFor("https://api2.cursor.sh", "custom"); got != "custom" {
		t.Fatal(got)
	}
}

func TestRefreshRequest(t *testing.T) {
	var got map[string]string
	var agent string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent = r.Header.Get("User-Agent")
		json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `{"access_token":"a","refresh_token":"r2","expires_in":60}`)
	}))
	defer s.Close()
	next, err := Refresh(context.Background(), s.Client(), s.URL, "", "r")
	if err != nil || next.AccessToken != "a" || next.RefreshToken != "r2" || next.ExpiresAtMS == 0 {
		t.Fatal(next, err)
	}
	// httptest listens on loopback, a development backend.
	if got["client_id"] != DevClientID || got["grant_type"] != "refresh_token" || got["refresh_token"] != "r" || agent != RefreshUserAgent {
		t.Fatal(got, agent)
	}
	if _, err = Refresh(context.Background(), s.Client(), s.URL, "custom", "r"); err != nil || got["client_id"] != "custom" {
		t.Fatal(got, err)
	}
}

// Like the desktop's runRefreshAccessToken, shouldLogout counts only in a
// successful response: a failed one is temporary.
func TestRefreshShouldLogoutOnlyOnSuccess(t *testing.T) {
	var status atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(int(status.Load()))
		fmt.Fprint(w, `{"shouldLogout":true}`)
	}))
	defer s.Close()
	for _, code := range []int{400, 401, 403, 500, 502} {
		status.Store(int32(code))
		_, err := Refresh(context.Background(), s.Client(), s.URL, "", "r")
		if err == nil || errors.Is(err, entity.ErrLogout) || err.Error() != fmt.Sprintf("refresh failed: HTTP %d", code) {
			t.Fatal(code, err)
		}
	}
	status.Store(http.StatusOK)
	if _, err := Refresh(context.Background(), s.Client(), s.URL, "", "r"); !errors.Is(err, entity.ErrLogout) {
		t.Fatal(err)
	}
}
