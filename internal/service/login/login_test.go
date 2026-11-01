package login

import (
	"bytes"
	"strings"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/instancelock"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/staterepo"
)

func TestWarnStaleConnect(t *testing.T) {
	store := testStore(t)
	alice := entity.Identity{AuthID: "alice", Email: "alice@example.invalid"}
	bob := entity.Identity{AuthID: "bob"}
	var out bytes.Buffer
	warnStaleConnect(store, alice, bob, &out)
	if out.Len() != 0 {
		t.Fatal("warned without a running connect:", out.String())
	}
	release, err := instancelock.Claim(store.Dir, "connect", instancelock.Holder{PID: 7})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	warnStaleConnect(store, alice, alice, &out)
	warnStaleConnect(store, entity.Identity{}, bob, &out)
	if out.Len() != 0 {
		t.Fatal("warned without an account change:", out.String())
	}
	warnStaleConnect(store, alice, bob, &out)
	if !strings.Contains(out.String(), "(pid 7) keeps serving alice@example.invalid") {
		t.Fatal(out.String())
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
