// Package entity holds the domain values of the CLI that are not wire
// formats: the user's identity, this machine, the computer credential and the
// permission mode. It does no I/O.
package entity

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Errors after which no token can be obtained until the user logs in again.
var (
	ErrLogout      = errors.New("session revoked; run grokbot-tunnel login")
	ErrNotLoggedIn = errors.New("not logged in; run grokbot-tunnel login")
)

// Identity holds the user's OAuth tokens and selected identity.
type Identity struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AuthID       string `json:"auth_id,omitempty"`
	TeamID       string `json:"team_id,omitempty"`
	Email        string `json:"email,omitempty"`
	// ExpiresAtMS is when AccessToken expires, if the server said so. Zero
	// means unknown; JWT access tokens carry their own expiry.
	ExpiresAtMS int64 `json:"expires_at_ms,omitempty"`
}

// AccountScope identifies the account and team that cached computer
// credentials belong to. It is stable across token rotation; emails are not
// used because they can be reassigned. It is empty if the account is unknown.
func (id Identity) AccountScope() string {
	sub := id.AuthID
	if sub == "" {
		sub = claims(id.AccessToken).Sub
	}
	if sub == "" {
		// Nothing stable identifies the account (access and refresh tokens
		// rotate), and an empty scope disables credential caching.
		return ""
	}
	sum := sha256.Sum256([]byte(sub + "\x00" + id.TeamID))
	return hex.EncodeToString(sum[:])
}

// claims decodes the payload of a JWT without verifying it. Malformed tokens
// yield zero claims.
func claims(token string) jwtClaims {
	var c jwtClaims
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return c
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return c
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return jwtClaims{}
	}
	return c
}

// Expiry returns when the access token expires: the exp claim of a JWT,
// else the expiry the server reported with it, else zero if unknown.
func (id Identity) Expiry() time.Time {
	if exp := claims(id.AccessToken).Exp; exp != 0 {
		return time.Unix(exp, 0)
	}
	if id.ExpiresAtMS != 0 {
		return time.UnixMilli(id.ExpiresAtMS)
	}
	return time.Time{}
}

// jwtClaims holds the JWT claims this client uses.
type jwtClaims struct {
	Exp int64  `json:"exp"`
	Sub string `json:"sub"`
}
