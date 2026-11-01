package sandapi

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
)

// fakeTokens hands out "t1" and records Rejected calls.
type fakeTokens struct {
	mu        sync.Mutex
	team      string
	rejected  []string
	rejectErr error
}

func (f *fakeTokens) Token(context.Context, bool) (string, error) { return "t1", nil }

func (f *fakeTokens) Rejected(_ context.Context, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected = append(f.rejected, token)
	return f.rejectErr
}

func (f *fakeTokens) Snapshot() entity.Identity { return entity.Identity{TeamID: f.team} }

// unauthenticated answers every call with a Connect Unauthenticated error,
// with an ErrorDetails of code if it is not zero.
func unauthenticated(t *testing.T, code uint64, calls *atomic.Int32, headers *http.Header) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		*headers = r.Header.Clone()
		details := ""
		if code != 0 {
			b := protowire.AppendVarint(protowire.AppendTag(nil, 1, protowire.VarintType), code)
			b = protowire.AppendVarint(protowire.AppendTag(b, 3, protowire.VarintType), 1) // is_expected
			details = fmt.Sprintf(`,"details":[{"type":%q,"value":%q}]`, errorDetailsType, base64.RawStdEncoding.EncodeToString(b))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"code":"unauthenticated","message":"rejected"%s}`, details)
	}))
	t.Cleanup(s.Close)
	return s
}

// Like the desktop's mOe, a rejected call reports the token but is not
// repeated, and errors about foreign credentials report nothing.
func TestBearerRejectedNotRepeated(t *testing.T) {
	for _, tc := range []struct {
		code   uint64
		report bool
	}{{0, true}, {2, true}, {45, true}, {46, false}, {47, false}, {48, false}, {49, false}, {50, true}} {
		t.Run(fmt.Sprint(tc.code), func(t *testing.T) {
			var calls atomic.Int32
			var headers http.Header
			s := unauthenticated(t, tc.code, &calls, &headers)
			tokens := &fakeTokens{team: "7"}
			client := rpc.NewDashboardServiceClient(s.Client(), s.URL, connect.WithInterceptors(Bearer(tokens)))
			_, err := client.GetMe(context.Background(), connect.NewRequest(&aiserverv1.GetMeRequest{}))
			if connect.CodeOf(err) != connect.CodeUnauthenticated || calls.Load() != 1 {
				t.Fatal(err, calls.Load())
			}
			if headers.Get("Authorization") != "Bearer t1" || headers.Get("X-Cursor-Team-Id") != "7" {
				t.Fatal(headers)
			}
			if got := len(tokens.rejected) == 1 && tokens.rejected[0] == "t1"; got != tc.report {
				t.Fatal(tokens.rejected)
			}
		})
	}
}

// A refresh that finds the session revoked replaces the call's error.
func TestBearerRejectedLogout(t *testing.T) {
	var calls atomic.Int32
	var headers http.Header
	s := unauthenticated(t, 0, &calls, &headers)
	tokens := &fakeTokens{rejectErr: entity.ErrLogout}
	client := rpc.NewDashboardServiceClient(s.Client(), s.URL, connect.WithInterceptors(Bearer(tokens)))
	if _, err := client.GetMe(context.Background(), connect.NewRequest(&aiserverv1.GetMeRequest{})); !errors.Is(err, entity.ErrLogout) {
		t.Fatal(err)
	}
	tokens = &fakeTokens{rejectErr: errors.New("network down")}
	client = rpc.NewDashboardServiceClient(s.Client(), s.URL, connect.WithInterceptors(Bearer(tokens)))
	if _, err := client.GetMe(context.Background(), connect.NewRequest(&aiserverv1.GetMeRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal(err)
	}
}

// Only a positive safe integer is sent as the team, as in the desktop.
func TestBearerTeamHeader(t *testing.T) {
	for team, want := range map[string]string{"7": "7", "007": "7", "0": "", "-1": "", "1.5": "", "x": "", "9007199254740992": ""} {
		var calls atomic.Int32
		var headers http.Header
		s := unauthenticated(t, 46, &calls, &headers)
		client := rpc.NewDashboardServiceClient(s.Client(), s.URL, connect.WithInterceptors(Bearer(&fakeTokens{team: team})))
		client.GetMe(context.Background(), connect.NewRequest(&aiserverv1.GetMeRequest{}))
		if got := headers.Get("X-Cursor-Team-Id"); got != want {
			t.Errorf("%q: got %q, want %q", team, got, want)
		}
	}
}

func TestErrorCode(t *testing.T) {
	b := protowire.AppendBytes(protowire.AppendTag(nil, 2, protowire.BytesType), []byte("details"))
	b = protowire.AppendVarint(protowire.AppendTag(b, 1, protowire.VarintType), 47)
	if got := errorCode(b); got != 47 {
		t.Fatal(got)
	}
	if got := errorCode([]byte{0x08}); got != 0 {
		t.Fatal(got)
	}
}
