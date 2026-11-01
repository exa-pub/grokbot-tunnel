package entity

import "testing"

func TestAccountScopeIgnoresRotatingTokens(t *testing.T) {
	if (Identity{AccessToken: "a"}).AccountScope() != "" {
		t.Fatal("scope without an account")
	}
	a := (Identity{AccessToken: "a", AuthID: "acc", TeamID: "1"}).AccountScope()
	if a == "" || a != (Identity{AccessToken: "b", AuthID: "acc", TeamID: "1"}).AccountScope() {
		t.Fatal("scope changed with the access token")
	}
	if a == (Identity{AuthID: "acc", TeamID: "2"}).AccountScope() {
		t.Fatal("teams share a scope")
	}
}
