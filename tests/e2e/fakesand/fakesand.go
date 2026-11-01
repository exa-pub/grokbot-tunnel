// Package fakesand is an in-process stand-in for the Grok Bot backend: the
// login poll, the profile calls and the user-computer channel (watch, poll,
// submit). Integration and end-to-end tests point the CLI at it with
// SAND_BACKEND_URL. It checks the header profile of each channel the way the
// backend sees it and reports violations on the test.
package fakesand

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
)

// Account is the identity every login gets.
const (
	AccountID = "fixture-account"
	Email     = "test@example.invalid"
)

// Server is a running fake backend.
type Server struct {
	rpc.UnimplementedDashboardServiceHandler
	rpc.UnimplementedGrokBotServiceHandler

	// URL is the base URL for SAND_BACKEND_URL.
	URL string
	// HoldConnected, if set before the CLI starts, holds back the connected
	// event of watch streams until it is closed.
	HoldConnected chan struct{}

	t       testing.TB
	mu      sync.Mutex
	queue   []*aiserverv1.GrokBotUserComputerQueuedRequest
	frames  []*aiserverv1.GrokBotUserComputerResponseFrame
	changed chan struct{} // closed and replaced when frames or watchers change
	watches int
	notify  []chan struct{} // one per open watch stream
}

// New starts a fake backend that stops when the test ends.
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{t: t, changed: make(chan struct{})}
	mux := http.NewServeMux()
	path, handler := rpc.NewDashboardServiceHandler(s)
	mux.Handle(path, handler)
	path, handler = rpc.NewGrokBotServiceHandler(s)
	mux.Handle(path, handler)
	mux.HandleFunc("/auth/poll", s.authPoll)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Env returns the environment that points the CLI at s.
func (s *Server) Env() []string {
	return []string{"SAND_BACKEND_URL=" + s.URL, "SAND_CURSOR_WEBSITE_URL=https://example.invalid"}
}

// Setenv points the CLI in this process at s for the duration of the test.
func (s *Server) Setenv(t testing.TB) {
	t.Helper()
	for _, kv := range s.Env() {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
}

// Enqueue queues request frames for the next poll and notifies open watch
// streams.
func (s *Server) Enqueue(frames ...*aiserverv1.GrokBotUserComputerRequestFrame) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range frames {
		s.queue = append(s.queue, &aiserverv1.GrokBotUserComputerQueuedRequest{Id: f.RequestId, Frame: f})
	}
	for _, c := range s.notify {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// Wait returns the submitted response frames once done accepts them, or
// fails the test after timeout.
func (s *Server) Wait(
	timeout time.Duration, done func([]*aiserverv1.GrokBotUserComputerResponseFrame) bool,
) []*aiserverv1.GrokBotUserComputerResponseFrame {
	s.t.Helper()
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		frames, changed := append([]*aiserverv1.GrokBotUserComputerResponseFrame(nil), s.frames...), s.changed
		s.mu.Unlock()
		if done(frames) {
			return frames
		}
		select {
		case <-changed:
		case <-deadline:
			s.t.Fatalf("fakesand: condition not met within %v; %d frames submitted", timeout, len(frames))
		}
	}
}

// WaitWatching waits until the CLI has a watch stream open.
func (s *Server) WaitWatching(timeout time.Duration) {
	s.t.Helper()
	deadline := time.After(timeout)
	for {
		s.mu.Lock()
		watching, changed := s.watches > 0, s.changed
		s.mu.Unlock()
		if watching {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			s.t.Fatalf("fakesand: no watch stream within %v", timeout)
		}
	}
}

// Exec returns an exec request frame carrying an agent.v1.ExecServerMessage
// in protojson.
func Exec(requestID, serverMessageJSON string) *aiserverv1.GrokBotUserComputerRequestFrame {
	return &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: requestID, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{
		Exec: &aiserverv1.GrokBotUserComputerExec{ServerMessageJson: serverMessageJSON},
	}}
}

// Upload returns a request frame that writes data to path.
func Upload(requestID, path string, data []byte) *aiserverv1.GrokBotUserComputerRequestFrame {
	return &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: requestID, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Upload{
		Upload: &aiserverv1.GrokBotUserComputerUpload{Path: path, Data: data},
	}}
}

// Download returns a request frame that reads path.
func Download(requestID, path string) *aiserverv1.GrokBotUserComputerRequestFrame {
	return &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: requestID, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Download{
		Download: &aiserverv1.GrokBotUserComputerDownload{Path: path},
	}}
}

// Client decodes the exec client message of f, or returns nil.
func Client(f *aiserverv1.GrokBotUserComputerResponseFrame) *agentv1.ExecClientMessage {
	if f.GetClient() == nil {
		return nil
	}
	msg := &agentv1.ExecClientMessage{}
	if protojson.Unmarshal([]byte(f.GetClient().MessageJson), msg) != nil {
		return nil
	}
	return msg
}

// Control decodes the exec control message of f, or returns nil.
func Control(f *aiserverv1.GrokBotUserComputerResponseFrame) *agentv1.ExecClientControlMessage {
	if f.GetControl() == nil {
		return nil
	}
	msg := &agentv1.ExecClientControlMessage{}
	if protojson.Unmarshal([]byte(f.GetControl().MessageJson), msg) != nil {
		return nil
	}
	return msg
}

func (s *Server) GetMe(
	_ context.Context, r *connect.Request[aiserverv1.GetMeRequest],
) (*connect.Response[aiserverv1.GetMeResponse], error) {
	if !strings.HasPrefix(r.Header().Get("Authorization"), "Bearer ") {
		s.t.Error("fakesand: profile bearer missing")
	}
	email := Email
	return connect.NewResponse(&aiserverv1.GetMeResponse{AuthId: AccountID, Email: &email}), nil
}

func (s *Server) GetUserPrivacyMode(
	context.Context, *connect.Request[aiserverv1.GetUserPrivacyModeRequest],
) (*connect.Response[aiserverv1.GetUserPrivacyModeResponse], error) {
	return connect.NewResponse(&aiserverv1.GetUserPrivacyModeResponse{PrivacyMode: aiserverv1.PrivacyMode_PRIVACY_MODE_NO_STORAGE}), nil
}

func (s *Server) RegisterSandMachine(
	_ context.Context, r *connect.Request[aiserverv1.RegisterSandMachineRequest],
) (*connect.Response[aiserverv1.RegisterSandMachineResponse], error) {
	if r.Header().Get("X-Cursor-Checksum") == "" {
		s.t.Error("fakesand: registration checksum missing")
	}
	return connect.NewResponse(&aiserverv1.RegisterSandMachineResponse{}), nil
}

func (s *Server) IssueGrokBotUserComputerCredential(
	context.Context, *connect.Request[aiserverv1.IssueGrokBotUserComputerCredentialRequest],
) (*connect.Response[aiserverv1.IssueGrokBotUserComputerCredentialResponse], error) {
	//nolint:gosec // a fake credential, not a secret
	return connect.NewResponse(&aiserverv1.IssueGrokBotUserComputerCredentialResponse{
		Credential: "fixture-credential", ExpiresAtMs: time.Now().Add(time.Hour).UnixMilli(),
	}), nil
}

// WatchGrokBotUserComputerRequests sends connected, then notify whenever a
// request is enqueued. The credential channel must not carry account headers.
func (s *Server) WatchGrokBotUserComputerRequests(
	ctx context.Context,
	r *connect.Request[aiserverv1.WatchGrokBotUserComputerRequestsRequest],
	stream *connect.ServerStream[aiserverv1.WatchGrokBotUserComputerRequestsEvent],
) error {
	for _, key := range []string{"Authorization", "X-Cursor-Checksum", "X-Ghost-Mode", "X-Request-Id"} {
		if r.Header().Get(key) != "" {
			s.t.Error("fakesand: credential channel header", key)
		}
	}
	if r.Header().Get("X-Sand-Box-Namespace") != "prod" {
		s.t.Error("fakesand: namespace missing")
	}
	notify := make(chan struct{}, 1)
	s.mu.Lock()
	s.notify = append(s.notify, notify)
	s.watches++
	s.signal()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, c := range s.notify {
			if c == notify {
				s.notify = append(s.notify[:i], s.notify[i+1:]...)
				break
			}
		}
		s.watches--
		s.signal()
	}()
	if s.HoldConnected != nil {
		select {
		case <-s.HoldConnected:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	connected := &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Connected{Connected: &aiserverv1.WatchGrokBotUserComputerConnected{}}
	if err := stream.Send(&aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: connected}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-notify:
			ev := &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Notify{Notify: &aiserverv1.WatchGrokBotUserComputerNotify{}}
			if err := stream.Send(&aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: ev}); err != nil {
				return err
			}
		}
	}
}

// PollGrokBotUserComputerRequests hands out every queued request once.
func (s *Server) PollGrokBotUserComputerRequests(
	context.Context, *connect.Request[aiserverv1.PollGrokBotUserComputerRequestsRequest],
) (*connect.Response[aiserverv1.PollGrokBotUserComputerRequestsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := &aiserverv1.PollGrokBotUserComputerRequestsResponse{Requests: s.queue}
	s.queue = nil
	return connect.NewResponse(out), nil
}

// SubmitGrokBotUserComputerResponses records the frames and accepts all of
// them.
func (s *Server) SubmitGrokBotUserComputerResponses(
	_ context.Context, r *connect.Request[aiserverv1.SubmitGrokBotUserComputerResponsesRequest],
) (*connect.Response[aiserverv1.SubmitGrokBotUserComputerResponsesResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frames = append(s.frames, r.Msg.Frames...)
	s.signal()
	return connect.NewResponse(&aiserverv1.SubmitGrokBotUserComputerResponsesResponse{
		AcceptedCount: uint32(len(r.Msg.Frames)), //nolint:gosec // bounded by the request size
	}), nil
}

// signal wakes Wait; s.mu must be held.
func (s *Server) signal() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// authPoll completes every login at once, with the header profile of the
// desktop's fetch.
func (s *Server) authPoll(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("User-Agent") != "undici" || r.Header.Get("Content-Type") != "application/json" {
		s.t.Errorf("fakesand: auth header profile %v", r.Header)
	}
	claims := fmt.Sprintf(`{"exp":%d,"sub":%q}`, time.Now().Add(time.Hour).Unix(), AccountID)
	token := "fixture." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".signature"
	if err := json.NewEncoder(w).Encode(map[string]string{
		"accessToken": token, "refreshToken": "fixture-refresh", "authId": AccountID,
	}); err != nil {
		s.t.Error(err)
	}
}
