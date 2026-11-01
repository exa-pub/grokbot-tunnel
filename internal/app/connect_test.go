package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/api/cli"
	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/instancelock"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/staterepo"
)

func TestConnectRefusesSecondInstance(t *testing.T) {
	store := testStore(t)
	release, err := instancelock.Claim(store.Dir, "connect", instancelock.Holder{PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = Connect(context.Background(), cli.ConnectOptions{Permission: "always", Label: "other"})
	if err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatal(err)
	}
	// The second instance must not touch the store, e.g. save its label.
	if _, err := os.Stat(filepath.Join(store.Dir, "machine.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("machine.json written:", err)
	}
}

func TestConnectWithoutLogin(t *testing.T) {
	store := testStore(t)
	err := Connect(context.Background(), cli.ConnectOptions{Permission: "always"})
	if !errors.Is(err, entity.ErrNotLoggedIn) {
		t.Fatal(err)
	}
	// A login wiped after the server revoked the session.
	if err = store.Save("auth", entity.Identity{}); err != nil {
		t.Fatal(err)
	}
	if err = Connect(context.Background(), cli.ConnectOptions{Permission: "always"}); !errors.Is(err, entity.ErrNotLoggedIn) {
		t.Fatal(err)
	}
	// The lock is released on failure.
	if _, held, err := instancelock.Probe(store.Dir, "connect"); held || err != nil {
		t.Fatal("lock held after Connect failed", err)
	}
}

func testStore(t *testing.T) *staterepo.Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	store, err := staterepo.Default()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// O3-1: Connect does not return while a token refresh it started is in
// flight, even when stopped meanwhile: exiting would drop the server's
// answer after it rotated the refresh token, and the next start would be
// logged out.
func TestConnectWaitsForRefresh(t *testing.T) {
	store := testStore(t)
	t.Setenv("NOTIFY_SOCKET", "")
	started, release := make(chan struct{}), make(chan struct{})
	var startOnce, releaseOnce sync.Once
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		startOnce.Do(func() { close(started) })
		<-release
		json.NewEncoder(w).Encode(map[string]string{"access_token": "a2", "refresh_token": "r2"})
	}))
	defer s.Close()
	defer releaseOnce.Do(func() { close(release) })
	t.Setenv("SAND_BACKEND_URL", s.URL)
	if err := store.Save("auth", entity.Identity{RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Connect(ctx, cli.ConnectOptions{Root: t.TempDir(), Permission: "always"}) }()
	select {
	case <-started: // Register is waiting for its token
	case err := <-done:
		t.Fatal("Connect returned before refreshing:", err)
	case <-time.After(5 * time.Second):
		t.Fatal("no refresh")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatal("Connect returned during the refresh:", err)
	case <-time.After(200 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Connect did not return after the refresh")
	}
	var saved entity.Identity
	if err := store.Load("auth", &saved); err != nil || saved.RefreshToken != "r2" {
		t.Fatal(saved, err)
	}
}
