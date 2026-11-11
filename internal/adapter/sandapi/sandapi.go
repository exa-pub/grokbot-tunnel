// Package sandapi provides the HTTP transport and auth interceptor shared by
// all RPC clients.
package sandapi

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/uuid"
)

// Version is the Sand desktop version whose protocol this client speaks.
const Version = "1.0.1"

// AuthUserAgent is the bundled Undici default: ain selects "undici" when
// neither __UNDICI_IS_NODE__ nor esbuildDetection is defined. The desktop
// polls the login with it; requests that set a User-Agent keep theirs.
const AuthUserAgent = "undici"

// clientVersionPattern is the desktop's owe: a release version, optionally
// with a pre-release suffix that is dropped. JavaScript's "." matches
// anything but line terminators.
var clientVersionPattern = regexp.MustCompile(`^(\d+\.\d+\.\d+)(?:-[^\n\r\x{2028}\x{2029}]+)?$`)

// ClientVersion normalizes a client version like the desktop's awe: the
// trimmed release version without a pre-release suffix, or Version if v is
// not one. The result is always safe in a header.
func ClientVersion(v string) string {
	m := clientVersionPattern.FindStringSubmatch(entity.TrimSpace(v))
	if m == nil {
		return Version
	}
	return m[1]
}

// HTTP1 returns a copy of base that speaks only HTTP/1.1 and offers no ALPN,
// like the desktop's connect-node clients and undici's fetch (allowH2:
// false), even if base was already set up for HTTP/2.
func HTTP1(base *http.Transport) *http.Transport {
	t := base.Clone()
	t.ForceAttemptHTTP2 = false
	var p http.Protocols
	p.SetHTTP1(true)
	t.Protocols = &p
	// An empty map disables HTTP/2; Clone copies one set up for it.
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if t.TLSClientConfig != nil {
		t.TLSClientConfig.NextProtos = nil
	}
	return t
}

// Profile selects which set of headers a Transport sends.
type Profile int

// Profiles keep auth REST, user RPC and credential RPC headers separate.
const (
	UserRPC     Profile = iota // dashboard and credential issuing, with the user's bearer token
	ComputerRPC                // watch/poll/submit, authenticated by the computer credential
	AuthREST                   // login polling and token refresh
)

// Checksum computes the X-Cursor-Checksum header: an obfuscated coarse
// timestamp followed by the machine ID.
func Checksum(machine string, now time.Time) string {
	// The protocol keeps only the low 32 bits of the timestamp.
	t := binary.BigEndian.AppendUint32(nil, uint32(now.UnixMilli()/1_000_000)) //nolint:gosec // truncation is intended
	b := []byte{t[2], t[3], t[0], t[1], t[2], t[3]}
	prev := byte(0xa5)
	for i := range b {
		b[i] = (b[i] ^ prev) + byte(i)
		prev = b[i]
	}
	return base64.RawURLEncoding.EncodeToString(b) + machine
}

// Transport adds the desktop client headers for its Profile to every request.
type Transport struct {
	Base    http.RoundTripper
	Machine string
	Version string
	Profile Profile
	Privacy *Privacy // sets X-Ghost-Mode for UserRPC; nil means always private
}

// computerStripped are user-scoped headers that must never reach the
// credential-authenticated computer channel.
var computerStripped = []string{"Authorization", "X-Cursor-Team-Id", "X-Cursor-Checksum", "X-Ghost-Mode", "X-Request-Id"}

// RoundTrip implements http.RoundTripper.
func (t Transport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if t.Profile == AuthREST {
		return t.roundTripREST(r)
	}
	v := ClientVersion(t.Version)
	h := r.Header
	h.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Sand/"+v+" Safari/537.36")
	h.Set("X-Cursor-Client-Type", "sand")
	h.Set("X-Cursor-Client-Version", v)
	h.Set("X-Cursor-Client-OS", runtime.GOOS)
	h.Set("X-Cursor-Client-Source", "sand-desktop")
	h.Set("X-Sand-Box-Namespace", "prod")
	if t.Profile == ComputerRPC {
		for _, key := range computerStripped {
			h.Del(key)
		}
		return t.Base.RoundTrip(r)
	}
	h.Set("X-Cursor-Checksum", Checksum(t.Machine, time.Now()))
	h.Set("X-Ghost-Mode", t.ghostMode(r))
	h.Set("X-Request-Id", uuid.New())
	return t.Base.RoundTrip(r)
}

func (t Transport) ghostMode(r *http.Request) string {
	// The privacy lookup itself goes through PrivacyHTTP, which has no Privacy.
	if t.Privacy == nil {
		return ghostOn
	}
	return t.Privacy.Ghost(r.Context())
}

// roundTripREST mimics Node's fetch: it negotiates compression itself, so the
// response is decoded here rather than by net/http.
func (t Transport) roundTripREST(r *http.Request) (*http.Response, error) {
	h := r.Header
	// Token refreshes set their own: the desktop sends them with another fetch.
	if h.Get("User-Agent") == "" {
		h.Set("User-Agent", AuthUserAgent)
	}
	h.Set("Accept", "*/*")
	h.Set("Accept-Language", "*")
	h.Set("Sec-Fetch-Mode", "cors")
	if r.URL.Scheme == "https" {
		h.Set("Accept-Encoding", "br, gzip, deflate")
	} else {
		h.Set("Accept-Encoding", "gzip, deflate")
	}
	resp, err := t.Base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	return decodeBody(resp)
}

// TokenSource hands out the user's access token; tokens.TokenSource
// implements it.
type TokenSource interface {
	Token(ctx context.Context, force bool) (string, error)
	// Rejected tells that the server refused token, so that a new one is
	// obtained if it is still the current one.
	Rejected(ctx context.Context, token string) error
	Snapshot() entity.Identity
}

// foreignAuthErrors are the ErrorDetails.Error codes (d3t) of Unauthenticated
// errors that are not about the user's token: GITHUB_NO_USER_CREDENTIALS,
// GITHUB_USER_NO_ACCESS, GITHUB_APP_NO_ACCESS and GITHUB_MULTIPLE_OWNERS.
var foreignAuthErrors = map[uint64]bool{46: true, 47: true, 48: true, 49: true}

// errorDetailsType is the detail the server explains errors with.
const errorDetailsType = "aiserver.v1.ErrorDetails"

// Bearer authenticates unary calls with the user's access token. Like the
// desktop's nOe, a call the server rejects as Unauthenticated, unless for a
// foreign credential, gets the token replaced but is not repeated: its
// error is returned, or the refresh's if the session is gone.
func Bearer(s TokenSource) connect.Interceptor {
	return connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, r connect.AnyRequest) (connect.AnyResponse, error) {
			tok, err := s.Token(ctx, false)
			if err != nil {
				return nil, err
			}
			r.Header().Set("Authorization", "Bearer "+tok)
			if team, ok := teamHeader(s.Snapshot().TeamID); ok {
				r.Header().Set("X-Cursor-Team-Id", team)
			}
			resp, err := next(ctx, r)
			if connect.CodeOf(err) != connect.CodeUnauthenticated || foreignAuth(err) {
				return resp, err
			}
			rerr := s.Rejected(ctx, tok)
			if errors.Is(rerr, entity.ErrLogout) || errors.Is(rerr, entity.ErrNotLoggedIn) {
				return nil, rerr
			}
			return resp, err
		}
	})
}

// foreignAuth reports whether an Unauthenticated error carries an
// ErrorDetails whose error is one of foreignAuthErrors.
func foreignAuth(err error) bool {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return false
	}
	for _, d := range ce.Details() {
		if d.Type() == errorDetailsType && foreignAuthErrors[errorCode(d.Bytes())] {
			return true
		}
	}
	return false
}

// errorCode returns field 1 (error) of an encoded ErrorDetails, or 0.
func errorCode(b []byte) uint64 {
	code := uint64(0)
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return 0
		}
		b = b[n:]
		if num == 1 && typ == protowire.VarintType {
			v, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return 0
			}
			code, b = v, b[m:]
			continue
		}
		if n = protowire.ConsumeFieldValue(num, typ, b); n < 0 {
			return 0
		}
		b = b[n:]
	}
	return code
}

// teamHeader returns the X-Cursor-Team-Id value for a saved team ID, if it
// is a positive safe integer: the desktop sends no other.
func teamHeader(team string) (string, bool) {
	n, err := strconv.ParseInt(team, 10, 64)
	if err != nil || n <= 0 || n > 1<<53-1 {
		return "", false
	}
	return strconv.FormatInt(n, 10), true
}
