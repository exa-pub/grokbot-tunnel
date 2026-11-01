package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
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
	if _, err := Poll(context.Background(), http.DefaultClient, "http://bad host", Login{UUID: "u", Verifier: "v"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestPollRidesOutTransientErrors(t *testing.T) {
	defer func(d time.Duration) { pollFirstDelay = d }(pollFirstDelay)
	pollFirstDelay = time.Millisecond
	var calls atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1, 2, 3:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 4, 5:
			w.WriteHeader(http.StatusTooManyRequests)
		case 6:
			w.WriteHeader(http.StatusNotFound)
		default:
			fmt.Fprint(w, `{"accessToken":"a","refreshToken":"r"}`)
		}
	}))
	defer s.Close()
	if a, err := Poll(context.Background(), s.Client(), s.URL, Login{UUID: "u", Verifier: "v"}); err != nil || a.AccessToken != "a" {
		t.Fatal(a, err)
	}
}

func TestPollTimeoutMessage(t *testing.T) {
	defer func(d, timeout time.Duration) { pollFirstDelay, pollTimeout = d, timeout }(pollFirstDelay, pollTimeout)
	pollFirstDelay, pollTimeout = time.Millisecond, 50*time.Millisecond
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) }))
	defer s.Close()
	if _, err := Poll(context.Background(), s.Client(), s.URL, Login{UUID: "u", Verifier: "v"}); err == nil || err.Error() != "login polling timed out" {
		t.Fatal(err)
	}
}
