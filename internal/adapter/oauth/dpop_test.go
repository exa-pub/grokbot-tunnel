package oauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
)

type proofParts struct {
	Header struct {
		Typ, Alg string
		Jwk      map[string]string
	}
	Claims struct {
		Jti, Htm, Htu string
		Iat           int64
	}
}

// verifyProof checks a proof as a server would and returns what it says.
func verifyProof(t *testing.T, proof string) proofParts {
	t.Helper()
	parts := strings.Split(proof, ".")
	require.Len(t, parts, 3)
	var p proofParts
	for i, v := range []any{&p.Header, &p.Claims} {
		b, err := base64.RawURLEncoding.DecodeString(parts[i])
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, v))
	}
	x, err := base64.RawURLEncoding.DecodeString(p.Header.Jwk["x"])
	require.NoError(t, err)
	y, err := base64.RawURLEncoding.DecodeString(p.Header.Jwk["y"])
	require.NoError(t, err)
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	require.NoError(t, err)
	require.Len(t, sig, 64)
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	require.True(t, ecdsa.Verify(pub, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])), "signature")
	return p
}

func TestProof(t *testing.T) {
	k, err := NewDPoPKey()
	require.NoError(t, err)
	now := time.Unix(1_800_000_000, 999_000_000)
	proof, err := k.Proof("post", "HTTPS://API2.Cursor.sh:443/auth/poll?uuid=x#f", now)
	require.NoError(t, err)
	p := verifyProof(t, proof)
	assert.Equal(t, "dpop+jwt", p.Header.Typ)
	assert.Equal(t, "ES256", p.Header.Alg)
	assert.Equal(t, map[string]string{"kty": "EC", "crv": "P-256", "x": p.Header.Jwk["x"], "y": p.Header.Jwk["y"]}, p.Header.Jwk)
	assert.Equal(t, "POST", p.Claims.Htm)
	assert.Equal(t, "https://api2.cursor.sh/auth/poll", p.Claims.Htu)
	assert.Equal(t, int64(1_800_000_000), p.Claims.Iat)
	assert.Len(t, p.Claims.Jti, 36)
	// The header's members in the desktop's order.
	head, _ := base64.RawURLEncoding.DecodeString(strings.Split(proof, ".")[0])
	assert.True(t, strings.HasPrefix(string(head), `{"typ":"dpop+jwt","alg":"ES256","jwk":{"kty":"EC","crv":"P-256","x":`), string(head))

	// RFC 7638: SHA-256 of the required members in lexical order.
	sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + p.Header.Jwk["x"] + `","y":"` + p.Header.Jwk["y"] + `"}`))
	assert.Equal(t, base64.RawURLEncoding.EncodeToString(sum[:]), k.Thumbprint())

	other, err := k.Proof("POST", "https://api2.cursor.sh/auth/poll", now)
	require.NoError(t, err)
	assert.NotEqual(t, p.Claims.Jti, verifyProof(t, other).Claims.Jti, "a new jti each time")
}

func TestProofURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://a.example":                  "https://a.example/",
		"http://A.example:80/x?q":            "http://a.example/x",
		"https://a.example:8443/oauth/token": "https://a.example:8443/oauth/token",
		"http://127.0.0.1:1234/auth/poll?":   "http://127.0.0.1:1234/auth/poll",
	} {
		got, err := proofURL(in)
		require.NoError(t, err)
		assert.Equal(t, want, got, in)
	}
}

func TestWrapRefresh(t *testing.T) {
	k, err := NewDPoPKey()
	require.NoError(t, err)
	assert.Equal(t, "plain", wrapRefresh("plain", nil))
	stored := wrapRefresh("rt", k)
	require.True(t, strings.HasPrefix(stored, "dpop:v1:"))
	token, key, err := unwrapRefresh(stored)
	require.NoError(t, err)
	assert.Equal(t, "rt", token)
	assert.Equal(t, k.Thumbprint(), key.Thumbprint())

	// The desktop's form, written by hand, with the key also as JSON text.
	ser := k.serialized()
	desktop := fmt.Sprintf(`{"refreshToken":"rt2","dpopKey":{"kind":"webcrypto-p256","v":1,"publicJwk":{"kty":"EC","crv":"P-256","x":%q,"y":%q},"privateKeyD":%q}}`,
		ser.PublicJwk.X, ser.PublicJwk.Y, ser.PrivateKeyD)
	b, err := json.Marshal(boundRefresh{RefreshToken: "rt2", DPoPKey: ser})
	require.NoError(t, err)
	assert.JSONEq(t, desktop, string(b))
	asText, err := json.Marshal(map[string]any{"refreshToken": "rt3", "dpopKey": desktop[strings.Index(desktop, `{"kind"`) : len(desktop)-1]})
	require.NoError(t, err)
	token, key, err = unwrapRefresh("dpop:v1:" + base64.RawURLEncoding.EncodeToString(asText))
	require.NoError(t, err)
	assert.Equal(t, "rt3", token)
	assert.Equal(t, k.Thumbprint(), key.Thumbprint())

	token, key, err = unwrapRefresh("session_rt_x")
	require.NoError(t, err)
	assert.Nil(t, key)
	assert.Equal(t, "session_rt_x", token)

	other, err := NewDPoPKey()
	require.NoError(t, err)
	mismatched := ser
	mismatched.PrivateKeyD = other.serialized().PrivateKeyD
	hardware := `{"kind":"hardware-p256","v":1,"provider":"secure-enclave","keyId":"k","publicJwk":{"kty":"EC","crv":"P-256","x":"` +
		ser.PublicJwk.X + `","y":"` + ser.PublicJwk.Y + `"}}`
	enc := func(v any) string {
		b, err := json.Marshal(v)
		require.NoError(t, err)
		return "dpop:v1:" + base64.RawURLEncoding.EncodeToString(b)
	}
	for name, stored := range map[string]string{
		"not base64url": "dpop:v1:a+b",
		"not JSON":      "dpop:v1:" + base64.RawURLEncoding.EncodeToString([]byte("x")),
		"no token":      enc(map[string]any{"dpopKey": ser}),
		"empty token":   enc(map[string]any{"refreshToken": "", "dpopKey": ser}),
		"no key":        enc(map[string]any{"refreshToken": "r"}),
		"other public":  enc(map[string]any{"refreshToken": "r", "dpopKey": mismatched}),
		"hardware":      enc(map[string]any{"refreshToken": "r", "dpopKey": json.RawMessage(hardware)}),
		"version text": enc(map[string]any{"refreshToken": "r", "dpopKey": map[string]any{"kind": "webcrypto-p256", "v": "1",
			"publicJwk": ser.PublicJwk, "privateKeyD": ser.PrivateKeyD}}),
		"version 2": enc(map[string]any{"refreshToken": "r", "dpopKey": map[string]any{"kind": "webcrypto-p256", "v": 2}}),
	} {
		_, _, err := unwrapRefresh(stored)
		assert.ErrorIs(t, err, ErrDPoPKey, name)
	}
}

func TestDeviceName(t *testing.T) {
	for in, want := range map[string]string{
		"host":                          "host",
		"  ho\u200bst\x07 \u00a0":       "host",
		"":                              "Cursor CLI",
		"\u200b":                        "Cursor CLI",
		strings.Repeat("a", 130):        strings.Repeat("a", 120),
		strings.Repeat("a", 119) + " b": strings.Repeat("a", 119),
	} {
		assert.Equal(t, want, DeviceName(in), in)
	}
}

// dpopServer is a backend that takes login intents and polls bound to their
// key, refusing the first proof with a clock 1 h ahead if skew is set.
type dpopServer struct {
	t     *testing.T
	jkt   atomic.Value
	skew  bool
	polls atomic.Int32
}

func (d *dpopServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/auth/login_intent":
		var v map[string]string
		assert.NoError(d.t, json.NewDecoder(r.Body).Decode(&v))
		assert.Equal(d.t, "linux", v["platform"])
		assert.Equal(d.t, "dev", v["deviceName"])
		assert.NotEmpty(d.t, v["uuid"])
		assert.NotEmpty(d.t, v["challenge"])
		d.jkt.Store(v["dpop_jkt"])
		fmt.Fprint(w, `{"shortCode":"X"}`)
	case "/auth/poll":
		n := d.polls.Add(1)
		assert.Equal(d.t, http.MethodPost, r.Method)
		assert.Empty(d.t, r.URL.RawQuery)
		var v map[string]string
		assert.NoError(d.t, json.NewDecoder(r.Body).Decode(&v))
		assert.NotEmpty(d.t, v["verifier"])
		p := verifyProof(d.t, r.Header.Get("DPoP"))
		assert.Equal(d.t, "http://"+r.Host+"/auth/poll", p.Claims.Htu)
		if d.skew && n == 1 {
			w.Header().Set("Date", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"invalid_dpop_proof"}`)
			return
		}
		if d.skew {
			assert.InDelta(d.t, time.Now().Add(time.Hour).Unix(), p.Claims.Iat, 5, "iat by the server's clock")
		}
		fmt.Fprint(w, `{"accessToken":"a","refreshToken":"session_rt_1","token_type":"DPoP"}`)
	default:
		http.NotFound(w, r)
	}
}

func TestPollDPoP(t *testing.T) {
	for _, skew := range []bool{false, true} {
		d := &dpopServer{t: t, skew: skew}
		s := httptest.NewServer(d)
		l := NewLogin("https://cursor.com")
		require.NoError(t, RegisterIntent(context.Background(), s.Client(), s.URL, &l, "dev"))
		require.NotNil(t, l.Key)
		assert.Equal(t, l.Key.Thumbprint(), d.jkt.Load())
		id, err := Poll(context.Background(), s.Client(), s.URL, l)
		require.NoError(t, err)
		assert.Equal(t, map[bool]int32{false: 1, true: 2}[skew], d.polls.Load(), "a refused proof is sent again at once")
		token, key, err := unwrapRefresh(id.RefreshToken)
		require.NoError(t, err)
		assert.Equal(t, "session_rt_1", token)
		assert.Equal(t, l.Key.Thumbprint(), key.Thumbprint())
		s.Close()
	}
}

func TestRegisterIntentFails(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"404":          func(w http.ResponseWriter, _ *http.Request) { http.NotFound(w, nil) },
		"no shortCode": func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"code":"x"}`) },
	} {
		s := httptest.NewServer(h)
		l := NewLogin("https://cursor.com")
		require.Error(t, RegisterIntent(context.Background(), s.Client(), s.URL, &l, "dev"), name)
		assert.Nil(t, l.Key, name)
		s.Close()
	}
}

// Tokens the server did not bind stay as they are, and bound tokens of a
// login without a key cannot carry one.
func TestParseLoginBound(t *testing.T) {
	k, err := NewDPoPKey()
	require.NoError(t, err)
	for body, bound := range map[string]bool{
		`{"accessToken":"a","refreshToken":"r","token_type":"Bearer"}`:                    false,
		`{"accessToken":"a","refreshToken":"r","tokenType":"DPoP"}`:                       true,
		`{"accessToken":"a","refreshToken":"r","token_type":null,"tokenType":"DPoP"}`:     true,
		`{"accessToken":"a","refreshToken":"r","token_type":"Bearer","tokenType":"DPoP"}`: false,
		`{"accessToken":"a","refreshToken":"session_rt_r","token_type":"Bearer"}`:         true,
		`{"accessToken":"a","refreshToken":"r","token_type":7}`:                           false,
	} {
		id, err := parseLogin([]byte(body), k)
		require.NoError(t, err, body)
		assert.Equal(t, bound, strings.HasPrefix(id.RefreshToken, "dpop:v1:"), body)
		id, err = parseLogin([]byte(body), nil)
		require.NoError(t, err, body)
		assert.False(t, strings.HasPrefix(id.RefreshToken, "dpop:v1:"), body)
	}
}

func TestRefreshDPoP(t *testing.T) {
	k, err := NewDPoPKey()
	require.NoError(t, err)
	var calls atomic.Int32
	var skew atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		p := verifyProof(t, r.Header.Get("DPoP"))
		assert.Equal(t, "http://"+r.Host+"/oauth/token", p.Claims.Htu)
		assert.Equal(t, "POST", p.Claims.Htm)
		var v map[string]string
		assert.NoError(t, json.NewDecoder(r.Body).Decode(&v))
		assert.Equal(t, "session_rt_old", v["refresh_token"])
		if skew.Load() && n == 1 {
			w.Header().Set("Date", time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat))
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":"invalid_dpop_proof"}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"a2","refresh_token":"session_rt_new"}`)
	}))
	defer s.Close()
	t.Cleanup(func() { refreshClock = serverClock{} })

	next, err := Refresh(context.Background(), s.Client(), s.URL, "", wrapRefresh("session_rt_old", k))
	require.NoError(t, err)
	assert.Equal(t, "a2", next.AccessToken)
	token, key, err := unwrapRefresh(next.RefreshToken)
	require.NoError(t, err)
	assert.Equal(t, "session_rt_new", token)
	assert.Equal(t, k.Thumbprint(), key.Thumbprint(), "the rotated token keeps the key")

	calls.Store(0)
	skew.Store(true)
	_, err = Refresh(context.Background(), s.Client(), s.URL, "", wrapRefresh("session_rt_old", k))
	require.NoError(t, err)
	assert.Equal(t, int32(2), calls.Load())

	// A refused proof without a clock that moved is not sent again.
	calls.Store(0)
	_, err = Refresh(context.Background(), s.Client(), s.URL, "", wrapRefresh("session_rt_old", k))
	require.EqualError(t, err, "refresh failed: HTTP 401")
	assert.Equal(t, int32(1), calls.Load())

	_, err = Refresh(context.Background(), s.Client(), s.URL, "", "dpop:v1:!")
	assert.True(t, errors.Is(err, entity.ErrLogout) && errors.Is(err, ErrDPoPKey), err)
}
