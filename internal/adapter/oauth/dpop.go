package oauth

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/uuid"
)

// DPoP binds a refresh token to a key, as the desktop does since 1.3
// (@anysphere/auth): the login and every refresh carry a proof signed with
// it (RFC 9449). On Linux the desktop keeps a software P-256 key
// ("webcrypto-p256"); this is the same key in the same serialized form.
const (
	dpopHeader       = "DPoP"
	dpopRefreshMagic = "dpop:v1:"
	// boundRefreshPrefix marks a refresh token the server bound to the key
	// ($ve), besides a token_type of DPoP.
	boundRefreshPrefix = "session_rt_"
	// skewRetry is how far the server's clock must have moved the offset
	// before a refused proof is sent again (Tzt).
	skewRetry = time.Second
)

// ErrDPoPKey is the desktop's DpopInvalidKeyError for a stored key that
// cannot be loaded: the session cannot be refreshed any more.
var ErrDPoPKey = errors.New("stored DPoP key is unreadable")

var jwkCoordinate = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// DPoPKey is a P-256 key for DPoP proofs.
type DPoPKey struct {
	priv *ecdsa.PrivateKey
}

// jwk is a public P-256 key, its members in the desktop's order.
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// serializedKey is the desktop's stored form of a software key (kL).
type serializedKey struct {
	Kind        string `json:"kind"`
	V           int    `json:"v"`
	PublicJwk   jwk    `json:"publicJwk"`
	PrivateKeyD string `json:"privateKeyD"`
}

// NewDPoPKey makes a key.
func NewDPoPKey() (*DPoPKey, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &DPoPKey{priv: priv}, nil
}

func coordinate(n *big.Int) string {
	return base64.RawURLEncoding.EncodeToString(n.FillBytes(make([]byte, 32)))
}

// Thumbprint is the key's JWK thumbprint (RFC 7638), the dpop_jkt of a
// login intent (Dve).
func (k *DPoPKey) Thumbprint() string {
	j := k.jwk()
	// The required members in lexical order.
	b, err := json.Marshal(struct {
		Crv string `json:"crv"`
		Kty string `json:"kty"`
		X   string `json:"x"`
		Y   string `json:"y"`
	}{j.Crv, j.Kty, j.X, j.Y})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Proof signs a DPoP proof for a request (_C): ES256, typ dpop+jwt, the
// public key in the header, and jti, htm, htu (without query and fragment)
// and iat.
func (k *DPoPKey) Proof(method, target string, now time.Time) (string, error) {
	htu, err := proofURL(target)
	if err != nil {
		return "", err
	}
	header, err := json.Marshal(struct {
		Typ string `json:"typ"`
		Alg string `json:"alg"`
		Jwk jwk    `json:"jwk"`
	}{"dpop+jwt", "ES256", k.jwk()})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(struct {
		Jti string `json:"jti"`
		Htm string `json:"htm"`
		Htu string `json:"htu"`
		Iat int64  `json:"iat"`
	}{uuid.New(), strings.ToUpper(method), htu, now.Unix()})
	if err != nil {
		return "", err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(input))
	r, s, err := ecdsa.Sign(rand.Reader, k.priv, digest[:])
	if err != nil {
		return "", err
	}
	// IEEE P1363: r and s, 32 bytes each.
	sig := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	return input + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (k *DPoPKey) jwk() jwk {
	return jwk{Kty: "EC", Crv: "P-256", X: coordinate(k.priv.X), Y: coordinate(k.priv.Y)}
}

// proofURL is the htu of a request as the desktop writes it (Lve): the URL
// as WHATWG URL prints it, without query and fragment.
func proofURL(target string) (string, error) {
	u, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if port := u.Port(); u.Scheme == "https" && port == "443" || u.Scheme == "http" && port == "80" {
		u.Host = u.Hostname()
	}
	if u.Path == "" {
		u.Path = "/"
	}
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	return u.String(), nil
}

func (k *DPoPKey) serialized() serializedKey {
	return serializedKey{
		Kind: "webcrypto-p256", V: 1, PublicJwk: k.jwk(),
		PrivateKeyD: base64.RawURLEncoding.EncodeToString(k.priv.D.FillBytes(make([]byte, 32))),
	}
}

// parseKey loads a stored key (ZZ, yC): the desktop's software key, given
// as an object or as its JSON text. Keys held by a hardware enclave of
// macOS or Windows cannot be loaded here.
func parseKey(raw json.RawMessage) (*DPoPKey, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		raw = json.RawMessage(text)
	}
	var probe struct {
		Kind      string          `json:"kind"`
		V         any             `json:"v"`
		PublicJwk json.RawMessage `json:"publicJwk"`
		D         *string         `json:"privateKeyD"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.V != float64(1) || probe.Kind != "webcrypto-p256" || probe.D == nil {
		return nil, ErrDPoPKey
	}
	x, y, ok := publicJwk(probe.PublicJwk)
	if !ok || !jwkCoordinate.MatchString(*probe.D) {
		return nil, ErrDPoPKey
	}
	d, err := base64.RawURLEncoding.DecodeString(*probe.D)
	if err != nil {
		return nil, ErrDPoPKey
	}
	priv, err := ecdh.P256().NewPrivateKey(d)
	if err != nil {
		return nil, ErrDPoPKey
	}
	// WebCrypto refuses a private key whose public half is another one.
	pt := priv.PublicKey().Bytes() // 0x04 || X || Y
	k := &DPoPKey{priv: &ecdsa.PrivateKey{
		PublicKey: ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(pt[1:33]), Y: new(big.Int).SetBytes(pt[33:])},
		D:         new(big.Int).SetBytes(d),
	}}
	if j := k.jwk(); j.X != x || j.Y != y {
		return nil, ErrDPoPKey
	}
	return k, nil
}

// publicJwk reads the public half of a stored key (vL): a P-256 JWK whose
// coordinates are 32 bytes in base64url, without a private member.
func publicJwk(raw json.RawMessage) (x, y string, ok bool) {
	var pub map[string]any
	if json.Unmarshal(raw, &pub) != nil || pub["kty"] != "EC" || pub["crv"] != "P-256" {
		return "", "", false
	}
	x, xOK := pub["x"].(string)
	y, yOK := pub["y"].(string)
	_, private := pub["d"]
	return x, y, xOK && yOK && !private && jwkCoordinate.MatchString(x) && jwkCoordinate.MatchString(y)
}

// boundRefresh is a refresh token and the key it is bound to, as the
// desktop stores them in one string (rY).
type boundRefresh struct {
	RefreshToken string        `json:"refreshToken"`
	DPoPKey      serializedKey `json:"dpopKey"`
}

// wrapRefresh stores a refresh token with its key: dpop:v1: and the
// base64url JSON of both. Without a key the token stays as it is.
func wrapRefresh(token string, key *DPoPKey) string {
	if key == nil {
		return token
	}
	//nolint:gosec // G117: this is the stored form of the token, which auth.json keeps
	b, err := json.Marshal(boundRefresh{RefreshToken: token, DPoPKey: key.serialized()})
	if err != nil {
		panic(err)
	}
	return dpopRefreshMagic + base64.RawURLEncoding.EncodeToString(b)
}

var base64URL = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// unwrapRefresh splits a stored refresh token into the token and its key
// (YL). A token without the prefix has no key; one with it that cannot be
// read fails with ErrDPoPKey.
func unwrapRefresh(stored string) (string, *DPoPKey, error) {
	rest, ok := strings.CutPrefix(stored, dpopRefreshMagic)
	if !ok {
		return stored, nil, nil
	}
	if !base64URL.MatchString(rest) {
		return "", nil, ErrDPoPKey
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(rest, "="))
	if err != nil {
		return "", nil, ErrDPoPKey
	}
	var v struct {
		RefreshToken *string         `json:"refreshToken"`
		DPoPKey      json.RawMessage `json:"dpopKey"`
	}
	if json.Unmarshal(b, &v) != nil || v.RefreshToken == nil || *v.RefreshToken == "" || v.DPoPKey == nil {
		return "", nil, ErrDPoPKey
	}
	key, err := parseKey(v.DPoPKey)
	if err != nil {
		return "", nil, err
	}
	return *v.RefreshToken, key, nil
}

// serverClock is the desktop's Su: the offset of the server's clock from
// ours, by the Date header of its answers, for the iat of proofs.
type serverClock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *serverClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *serverClock) current() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.offset
}

// observe takes the offset from a Date header, reporting whether there was
// one to take.
func (c *serverClock) observe(h http.Header) bool {
	t, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset = time.Until(t)
	return true
}

// refreshClock is the clock of token refreshes, kept for the process like
// the desktop's dpopServerClock.
var refreshClock serverClock

// invalidProof reports whether a body is {"error":"invalid_dpop_proof"}.
func invalidProof(body []byte) bool {
	var v struct {
		Error any `json:"error"`
	}
	return json.Unmarshal(body, &v) == nil && v.Error == "invalid_dpop_proof"
}

// shifted takes the offset from a Date header and reports whether it moved
// by skewRetry or more, the condition for sending a refused proof again
// (IL, Ewe).
func (c *serverClock) shifted(h http.Header) bool {
	before := c.current()
	if !c.observe(h) {
		return false
	}
	moved := c.current() - before
	return moved >= skewRetry || moved <= -skewRetry
}

// retryProof reports whether a refresh whose proof the server refused is
// worth sending again: only a 401 moves the clock of refreshes (Ewe).
func retryProof(c *serverClock, status int, header http.Header, body []byte) bool {
	return status == http.StatusUnauthorized && c.shifted(header) && invalidProof(body)
}

// response is a read answer.
type response struct {
	status int
	header http.Header
	body   []byte
}

func send(h *http.Client, req *http.Request) (response, error) {
	r, err := h.Do(req)
	if err != nil {
		return response{}, err
	}
	defer r.Body.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(limitReader(r))
	return response{status: r.StatusCode, header: r.Header, body: buf.Bytes()}, err
}

func errHTTP(status int) error { return fmt.Errorf("HTTP %d", status) }
