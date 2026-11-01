package install

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/instancelock"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/staterepo"
)

func TestConnectArgs(t *testing.T) {
	root := t.TempDir()
	t.Chdir(filepath.Dir(root))
	opts := Options{Root: filepath.Base(root), Permission: "always", Label: "my box", Debug: true, ClientVersion: "0.1.0"}
	args, err := opts.connectArgs()
	want := []string{"--permission", "always", "--root", root, "--label", "my box", "--debug", "--client-version", "0.1.0"}
	if err != nil || !slices.Equal(args, want) {
		t.Fatal(args, err)
	}
	if args, err = (Options{Permission: "ask"}).connectArgs(); err != nil || len(args) != 2 {
		t.Fatal(args, err)
	}
	for _, c := range []Options{
		{},
		{Permission: "sometimes"},
		{Permission: "always", Root: filepath.Join(root, "missing")},
	} {
		if _, err := c.connectArgs(); err == nil {
			t.Errorf("%+v accepted", c)
		}
	}
}

func TestCheckInstallable(t *testing.T) {
	store := testStore(t)
	if err := checkInstallable(store); err == nil || !strings.Contains(err.Error(), "grokbot-tunnel login") {
		t.Fatal(err)
	}
	if err := store.Save("auth", entity.Identity{RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	if err := checkInstallable(store); err != nil {
		t.Fatal(err)
	}
	release, err := instancelock.Claim(store.Dir, "connect", instancelock.Holder{PID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := checkInstallable(store); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatal(err)
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
