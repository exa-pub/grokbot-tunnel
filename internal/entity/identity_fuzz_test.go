package entity

import (
	"regexp"
	"testing"
)

var scopeHex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// The account scope of any tokens is empty or a SHA-256, and depends only on
// the account and team, not on the rest of the token.
func FuzzAccountScope(f *testing.F) {
	f.Add("", "eyJhbGciOiJub25lIn0.eyJzdWIiOiJ1c2VyIn0.", "team")
	f.Add("auth", "not.a.jwt", "")
	f.Add("", "a.b.c", "t")
	f.Fuzz(func(t *testing.T, authID, access, team string) {
		id := Identity{AuthID: authID, AccessToken: access, TeamID: team}
		scope := id.AccountScope()
		if scope != "" && !scopeHex.MatchString(scope) {
			t.Fatalf("scope %q", scope)
		}
		if authID != "" {
			other := Identity{AuthID: authID, AccessToken: "x" + access, RefreshToken: "r", TeamID: team}
			if other.AccountScope() != scope {
				t.Fatal("scope depends on the access token")
			}
		}
	})
}
