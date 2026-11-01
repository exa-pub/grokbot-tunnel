package app

import (
	"net/http"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/adapter/oauth"
	"github.com/exa-pub/grokbot-tunnel/internal/adapter/sandapi"
	"github.com/exa-pub/grokbot-tunnel/internal/entity"
)

// Like the desktop's clients, auth REST and RPC speak only HTTP/1.1.
func TestClientsSpeakHTTP1(t *testing.T) {
	testStore(t)
	e, err := newEnv("")
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]*http.Client{"rest": e.rest, "computer": e.computer, "rpc": e.rpc} {
		st, ok := c.Transport.(sandapi.Transport)
		if !ok {
			t.Fatalf("%s: %T", name, c.Transport)
		}
		base, ok := st.Base.(*http.Transport)
		if !ok || base.Protocols == nil || base.Protocols.HTTP2() || !base.Protocols.HTTP1() || base.ForceAttemptHTTP2 {
			t.Fatalf("%s: %+v", name, st.Base)
		}
	}
}

// The OAuth client follows the backend and SAND_AUTH_CLIENT_ID, as in the
// desktop's zh.
func TestTokensClientID(t *testing.T) {
	testStore(t)
	for _, tc := range []struct{ backend, override, want string }{
		{"", "", oauth.ClientID},
		{"http://localhost:4000", "", oauth.DevClientID},
		{"", "custom", "custom"},
	} {
		t.Setenv("SAND_BACKEND_URL", tc.backend)
		t.Setenv("SAND_AUTH_CLIENT_ID", tc.override)
		e, err := newEnv("")
		if err != nil {
			t.Fatal(err)
		}
		if got := e.user(entity.Identity{}).tokens.ClientID; got != tc.want {
			t.Errorf("%+v: got %s", tc, got)
		}
	}
}
