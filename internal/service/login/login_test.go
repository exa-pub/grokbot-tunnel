package login

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/oauth"

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

// A backend that does not take the login intent gets the login without a
// DPoP key, as the desktop falls back to it.
func TestPrepareFallsBack(t *testing.T) {
	var intents atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/login_intent" {
			intents.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if r.URL.Path == "/auth/poll" && r.Method == http.MethodGet {
			fmt.Fprint(w, `{"accessToken":"a","refreshToken":"session_rt_r","token_type":"DPoP"}`)
			return
		}
		t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
	}))
	defer s.Close()
	l := Login{HTTP: s.Client(), Base: s.URL, Website: "https://cursor.com", DPoP: true}
	pending, err := l.prepare(context.Background())
	if err != nil || pending.Key != nil || intents.Load() != 1 {
		t.Fatal(pending, err, intents.Load())
	}
	id, err := oauth.Poll(context.Background(), s.Client(), s.URL, pending)
	if err != nil || id.RefreshToken != "session_rt_r" {
		t.Fatal(id, err)
	}
	l.DPoP = false
	if pending, err = l.prepare(context.Background()); err != nil || pending.Key != nil || intents.Load() != 1 {
		t.Fatal("a development client asked for an intent:", err, intents.Load())
	}
	l.DPoP = true
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := l.prepare(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("a canceled login falls back:", err)
	}
}
