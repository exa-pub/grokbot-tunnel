package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
