package computer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/anypb"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	rpc "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1/aiserverv1connect"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/repository/staterepo"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/admission"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/replies"
)

// testHandler runs every request until it is canceled. Requests whose ID
// starts with "close" then report a stream close; those starting with
// "stuck" ignore the cancellation until the test ends; those starting with
// "flood" send output until the sink fails.
type testHandler struct{ end <-chan struct{} }

func (h testHandler) Handle(ctx context.Context, f *requestFrame, _ bool, sink frames.Sink) error {
	if strings.HasPrefix(f.RequestId, "flood") {
		output := &responseFrame{RequestId: f.RequestId, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{
			Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: `{"shellStream":{"stdout":{"data":"x"}}}`},
		}}
		for {
			if err := sink(output); err != nil {
				return err
			}
		}
	}
	if strings.HasPrefix(f.RequestId, "stuck") {
		<-h.end
		return nil
	}
	<-ctx.Done()
	if strings.HasPrefix(f.RequestId, "close") {
		return sink(closeFrame(f.RequestId))
	}
	return nil
}

func testSession(t *testing.T) *session {
	t.Helper()
	end := make(chan struct{})
	t.Cleanup(func() { close(end) })
	pv := &Provider{
		Handler:  testHandler{end: end},
		Store:    &staterepo.Store{Dir: t.TempDir()},
		Standing: func() entity.Mode { return entity.ModeAlways },
		Now:      func() time.Time { return time.Unix(1000, 0) },
		Jitter:   func(time.Duration) time.Duration { return 0 },
	}
	s := newSession(context.Background(), pv)
	t.Cleanup(s.close)
	return s
}

// withCredential gives s a credential valid for an hour.
func withCredential(s *session, name string, server bool) {
	s.cred.Restore(entity.Credential{Credential: name, ExpiresAtMS: s.pv.now().Add(time.Hour).UnixMilli(), ServerAuthoritative: server})
}

func execFrame(id string) *requestFrame {
	return &requestFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{
		Exec: &aiserverv1.GrokBotUserComputerExec{ServerMessageJson: `{"readArgs":{"path":"x"}}`},
	}}
}

func cancelFrame(id string) *requestFrame {
	return &requestFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Cancel{
		Cancel: &aiserverv1.GrokBotUserComputerCancel{},
	}}
}

func closeFrame(id string) *responseFrame {
	return &responseFrame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Control{
		Control: &aiserverv1.GrokBotUserComputerControlMessage{MessageJson: `{"streamClose":{"id":3}}`},
	}}
}

// occupy starts n requests named prefix0… that run until canceled; s needs a
// credential.
func occupy(t *testing.T, s *session, prefix string, n int) {
	t.Helper()
	before := s.admit.Active()
	for i := range n {
		s.accept(execFrame(fmt.Sprint(prefix, i)), s.cred.Current().ServerAuthoritative)
	}
	if s.admit.Active() != before+n {
		t.Fatalf("%d requests started, want %d", s.admit.Active()-before, n)
	}
}

// stubCloud is a configurable server for session-level tests.
type stubCloud struct {
	rpc.UnimplementedGrokBotServiceHandler
	mu                     sync.Mutex
	issues, submits, polls int
	ttl                    time.Duration
	failIssue, failSubmit  bool
	failPoll               bool
	submitted              []*responseFrame
	block                  chan struct{} // holds submits until closed
	holdFirst              chan struct{} // holds the first submit until closed
	held                   chan struct{} // closed once the first submit waits on holdFirst
	holding                bool
	unauthenticated        bool                                           // submits are rejected
	rejectWatch            bool                                           // watches are rejected
	connect                bool                                           // watches report connected
	blockIssue             chan struct{}                                  // holds issues until closed
	requests               []*aiserverv1.GrokBotUserComputerQueuedRequest // returned by the next poll
	pollAcks               [][]string                                     // ack IDs of every poll
	pollLimits             []uint32
	removed                int           // issues after this many say the computer was removed; 0: none, -1: all
	accept                 int           // submits report this many frames accepted if not 0
	withdrawn              []string      // credentials of the presence withdrawals
	submitTimeout          time.Duration // deadline of the last submit
	pollTimeout            time.Duration // deadline of the last poll
}

func timeout(ctx context.Context) time.Duration {
	d, ok := ctx.Deadline()
	if !ok {
		return 0
	}
	return time.Until(d)
}

// removedError is the server's answer for a computer removed from the account.
func removedError() error {
	err := connect.NewError(connect.CodeFailedPrecondition, errors.New("computer removed"))
	detail, derr := connect.NewErrorDetail(&anypb.Any{TypeUrl: "type.googleapis.com/aiserver.v1.GrokBotUserComputerRemovedDetail"})
	if derr != nil {
		panic(derr)
	}
	err.AddDetail(detail)
	return err
}

func (c *stubCloud) WithdrawGrokBotUserComputerPresence(
	_ context.Context, r *connect.Request[aiserverv1.WithdrawGrokBotUserComputerPresenceRequest],
) (*connect.Response[aiserverv1.WithdrawGrokBotUserComputerPresenceResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.withdrawn = append(c.withdrawn, r.Msg.Credential)
	return connect.NewResponse(&aiserverv1.WithdrawGrokBotUserComputerPresenceResponse{Withdrawn: true}), nil
}

func (c *stubCloud) IssueGrokBotUserComputerCredential(
	ctx context.Context, _ *connect.Request[aiserverv1.IssueGrokBotUserComputerCredentialRequest],
) (*connect.Response[aiserverv1.IssueGrokBotUserComputerCredentialResponse], error) {
	if c.blockIssue != nil {
		select {
		case <-c.blockIssue:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.issues++
	if c.failIssue {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	if c.removed != 0 && c.issues > c.removed {
		return nil, removedError()
	}
	return connect.NewResponse(&aiserverv1.IssueGrokBotUserComputerCredentialResponse{
		Credential: fmt.Sprint(c.issues), ExpiresAtMs: time.Now().Add(c.ttl).UnixMilli(),
	}), nil
}

func (c *stubCloud) SubmitGrokBotUserComputerResponses(
	ctx context.Context, r *connect.Request[aiserverv1.SubmitGrokBotUserComputerResponsesRequest],
) (*connect.Response[aiserverv1.SubmitGrokBotUserComputerResponsesResponse], error) {
	if c.block != nil {
		<-c.block
	}
	c.mu.Lock()
	if hold := c.holdFirst; hold != nil && !c.holding {
		c.holding = true
		close(c.held)
		c.mu.Unlock()
		<-hold
		c.mu.Lock()
	}
	defer c.mu.Unlock()
	c.submits++
	c.submitTimeout = timeout(ctx)
	if c.unauthenticated {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("expired"))
	}
	if c.failSubmit {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	c.submitted = append(c.submitted, r.Msg.Frames...)
	accepted := len(r.Msg.Frames)
	if c.accept != 0 {
		accepted = c.accept
	}
	return connect.NewResponse(&aiserverv1.SubmitGrokBotUserComputerResponsesResponse{AcceptedCount: uint32(accepted)}), nil //nolint:gosec // small
}

func (c *stubCloud) PollGrokBotUserComputerRequests(
	ctx context.Context, r *connect.Request[aiserverv1.PollGrokBotUserComputerRequestsRequest],
) (*connect.Response[aiserverv1.PollGrokBotUserComputerRequestsResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.polls++
	c.pollTimeout = timeout(ctx)
	c.pollAcks = append(c.pollAcks, r.Msg.AckIds)
	c.pollLimits = append(c.pollLimits, r.Msg.Limit)
	if c.failPoll {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	out := &aiserverv1.PollGrokBotUserComputerRequestsResponse{Requests: c.requests}
	c.requests = nil
	return connect.NewResponse(out), nil
}

func (c *stubCloud) WatchGrokBotUserComputerRequests(
	ctx context.Context, _ *connect.Request[aiserverv1.WatchGrokBotUserComputerRequestsRequest],
	stream *connect.ServerStream[aiserverv1.WatchGrokBotUserComputerRequestsEvent],
) error {
	if c.rejectWatch {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("rejected"))
	}
	if c.connect {
		ev := &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Connected{Connected: &aiserverv1.WatchGrokBotUserComputerConnected{}}
		if err := stream.Send(&aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: ev}); err != nil {
			return err
		}
	}
	<-ctx.Done()
	return ctx.Err()
}

func (c *stubCloud) counts() (issues, submits, polls int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.issues, c.submits, c.polls
}

// stubServer serves c over HTTP until the test ends.
func stubServer(t *testing.T, c *stubCloud) *httptest.Server {
	t.Helper()
	path, h := rpc.NewGrokBotServiceHandler(c)
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// stubSession returns a session talking to c whose clock is controlled by *now.
func stubSession(t *testing.T, c *stubCloud, now *time.Time) *session {
	t.Helper()
	srv := stubServer(t, c)
	s := testSession(t)
	*now = time.Now()
	s.pv.Now = func() time.Time { return *now }
	cl := rpc.NewGrokBotServiceClient(srv.Client(), srv.URL)
	s.pv.User, s.pv.Channel = cl, cl
	return s
}

// settle waits for the issue, submit and poll in flight, applying their
// results like Run does, and returns the first error.
func settle(t *testing.T, s *session) error {
	t.Helper()
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for s.out.Submitting() || s.poll.Polling() || s.cred.Issuing() {
		select {
		case r := <-s.issued:
			keep(s.onIssued(r))
		case r := <-s.submitted:
			s.onSubmitted(r)
		case r := <-s.polled:
			s.onPolled(r)
		case <-time.After(5 * time.Second):
			t.Fatal("RPC did not finish")
		}
	}
	return first
}

// tick runs onTick and settles the RPCs it started.
func tick(t *testing.T, s *session) error {
	t.Helper()
	s.onTick()
	return settle(t, s)
}

// A request canceled while it waits is closed and forgotten, its unsent
// frames are dropped, and a redelivery does not start it again.
func TestCancelWaitingRequest(t *testing.T) {
	s := testSession(t)
	withCredential(s, "c", false)
	occupy(t, s, "busy", admission.MaxActive)
	s.accept(execFrame("canceled"), false)
	if s.admit.Backlog() != 1 {
		t.Fatal("missing backlog")
	}
	s.out.Add(&responseFrame{RequestId: "canceled", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{
		Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: "old"},
	}})
	s.accept(cancelFrame("canceled"), false)
	pending := s.out.Pending()
	if s.admit.Backlog() != 0 || len(pending) != 1 || pending[0].GetControl() == nil {
		t.Fatal("cancel did not discard backlog/replies", pending)
	}
	s.accept(execFrame("canceled"), false)
	if s.admit.Backlog() != 0 {
		t.Fatal("canceled work restarted")
	}
}

func TestCredentialCacheScopeAndInvalidation(t *testing.T) {
	store := &staterepo.Store{Dir: t.TempDir()}
	now := time.Now()
	valid := entity.Credential{Credential: "cached", AccountScope: "account", Backend: "https://api.example", MachineID: "machine", ExpiresAtMS: now.Add(time.Hour).UnixMilli()}
	if err := store.Save("computer", valid); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{"account", "other"} {
		pv := &Provider{Store: store, AccountScope: scope, Backend: valid.Backend, Machine: entity.Machine{MachineID: valid.MachineID}}
		s := newSession(context.Background(), pv)
		if (s.cred.Current().Credential != "") != (scope == "account") {
			t.Fatal("cross-account cache use")
		}
		if scope == "account" {
			s.invalidate("", connect.NewError(connect.CodeUnauthenticated, errors.New("expired")))
			var c entity.Credential
			store.Load("computer", &c)
			if c.Credential != "" {
				t.Fatal("rejected credential persisted")
			}
			store.Save("computer", valid)
		}
		s.close()
	}
	legacy := valid
	legacy.AccountScope = ""
	store.Save("computer", legacy)
	s := newSession(context.Background(), &Provider{Store: store, AccountScope: "account", Backend: valid.Backend, Machine: entity.Machine{MachineID: valid.MachineID}})
	defer s.close()
	if s.cred.Current().Credential != "" {
		t.Fatal("legacy cache accepted")
	}
}

func TestPartialSubmitDrainsAndPreservesOrder(t *testing.T) {
	var now time.Time
	c := &stubCloud{}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)
	for i := range 7 {
		s.out.Add(&responseFrame{RequestId: "a", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_File{
			File: &aiserverv1.GrokBotUserComputerFile{Seq: uint32(i), Data: []byte("x")}, //nolint:gosec // small
		}})
	}
	if err := s.submit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, submits, _ := c.counts(); len(s.out.Pending()) != 0 || submits != 7 {
		t.Fatal(len(s.out.Pending()), submits)
	}
	for i, f := range c.submitted {
		if f.GetFile().GetSeq() != uint32(i) { //nolint:gosec // small
			t.Fatal(c.submitted)
		}
	}
}

// Invariant 3: with 8 requests running and 64 waiting, one more is refused;
// shutdown closes the waiting ones and waits for the running ones.
func TestInflightLimitAndShutdownCancel(t *testing.T) {
	s := testSession(t)
	withCredential(s, "c", false)
	occupy(t, s, "busy", admission.MaxActive)
	for i := range admission.MaxBacklog + 1 {
		s.accept(execFrame(fmt.Sprint(i)), false)
	}
	if s.admit.Backlog() != admission.MaxBacklog || len(s.admit.Tasks()) != admission.MaxActive+admission.MaxBacklog || len(s.out.Pending()) != 1 {
		t.Fatal(s.admit.Backlog(), len(s.admit.Tasks()), len(s.out.Pending()))
	}
	s.cred.Restore(entity.Credential{}) // nothing to submit with
	s.shutdown()
	if len(s.admit.Tasks()) != 0 || s.admit.Active() != 0 || len(s.out.Pending()) != 1+admission.MaxBacklog {
		t.Fatal("queued tasks not canceled", len(s.admit.Tasks()), len(s.out.Pending()))
	}
}

// Invariant 4: an unrecognized frame is answered even when busy, once.
func TestUnrecognizedFrameAnsweredWhenBusy(t *testing.T) {
	s := testSession(t)
	withCredential(s, "c", false)
	occupy(t, s, "busy", admission.MaxActive)
	for i := range admission.MaxBacklog {
		s.accept(execFrame(fmt.Sprint(i)), false)
	}
	url := &requestFrame{RequestId: "url", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_PutFileToUrl{
		PutFileToUrl: &aiserverv1.GrokBotUserComputerPutFileToUrl{},
	}}
	s.accept(url, false)
	pending := s.out.Pending()
	if len(pending) != 1 || !frames.Closes(pending[0]) || !strings.Contains(pending[0].GetControl().GetMessageJson(), "does not understand that request frame") {
		t.Fatal(pending)
	}
	s.accept(url, false)
	s.accept(&requestFrame{}, false)
	if len(s.out.Pending()) != 1 {
		t.Fatal("answered twice, or without a request id")
	}
}

// Watch events that ask for work schedule a poll; connecting proves the
// credential.
func TestEventsRequestPoll(t *testing.T) {
	s := testSession(t)
	gen := s.stream.Start(s.pv.now(), "always")
	s.onEvent(watched{generation: gen - 1, event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent{
		Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Notify{Notify: &aiserverv1.WatchGrokBotUserComputerNotify{}},
	}})
	if s.poll.Needed() {
		t.Fatal("stale event used")
	}
	s.onEvent(watched{generation: gen, event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent{
		Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Notify{Notify: &aiserverv1.WatchGrokBotUserComputerNotify{}},
	}})
	if !s.poll.Needed() {
		t.Fatal("notify ignored")
	}
}

// Failed submit rounds never end the session (bug B3): keep-alives are
// dropped, the closing frame is kept and retried with a growing delay
// (bug B4).
func TestFailedSubmitRoundKeepsSession(t *testing.T) {
	var now time.Time
	c := &stubCloud{failSubmit: true}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)
	tasks := map[string]bool{}
	for i := range 72 {
		tasks[fmt.Sprint(i)] = true
	}
	s.out.Add(closeFrame("closed"))
	s.out.KeepAlive(tasks, s.admit.Canceled)
	for range 2 * replies.SubmitAttempts {
		now = now.Add(time.Second)
		s.startSubmit()
		if err := settle(t, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, submits, _ := c.counts(); submits != 2*replies.SubmitAttempts || len(s.out.Pending()) != 1 {
		t.Fatal(submits, len(s.out.Pending()))
	}
	if s.out.Pending()[0].RequestId != "closed" {
		t.Fatal("closing frame dropped")
	}
}

func TestShortCredentialRenewedHalfway(t *testing.T) {
	var now time.Time
	c := &stubCloud{ttl: 4 * time.Minute}
	s := stubSession(t, c, &now)
	for range 5 {
		if err := tick(t, s); err != nil {
			t.Fatal(err)
		}
	}
	if issues, _, _ := c.counts(); issues != 1 {
		t.Fatalf("credential reissued %d times", issues)
	}
	now = now.Add(time.Minute)
	tick(t, s)
	if issues, _, _ := c.counts(); issues != 1 {
		t.Fatal("renewed too early")
	}
	now = now.Add(61 * time.Second)
	tick(t, s)
	if issues, _, _ := c.counts(); issues != 2 {
		t.Fatal("not renewed halfway", issues)
	}
}

func TestFailedRenewalKeepsSessionWorking(t *testing.T) {
	var now time.Time
	c := &stubCloud{failIssue: true}
	s := stubSession(t, c, &now)
	s.cred.Restore(entity.Credential{Credential: "c", ExpiresAtMS: now.Add(4 * time.Minute).UnixMilli()})
	s.stream.Start(now, "always")
	s.out.Add(&responseFrame{RequestId: "r", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_MessagesError{
		MessagesError: &aiserverv1.GrokBotUserComputerMessagesError{},
	}})
	for range 3 {
		if err := tick(t, s); err != nil {
			t.Fatal(err)
		}
	}
	issues, submits, polls := c.counts()
	if issues != 1 || submits != 1 || len(s.out.Pending()) != 0 {
		t.Fatalf("issues=%d submits=%d pending=%d", issues, submits, len(s.out.Pending()))
	}
	if !s.stream.NextDial().IsZero() || !s.stream.Alive() {
		t.Fatal("renewal failure delayed the watch", s.stream.NextDial())
	}
	s.poll.Request()
	tick(t, s)
	if _, _, p := c.counts(); p != polls+1 {
		t.Fatal("poll blocked by failed renewal")
	}
	now = now.Add(29 * time.Second)
	tick(t, s)
	if issues, _, _ := c.counts(); issues != 1 {
		t.Fatal("renewal retried before backoff", issues)
	}
	now = now.Add(time.Second)
	tick(t, s)
	if issues, _, _ := c.counts(); issues != 2 {
		t.Fatal("renewal not retried after backoff", issues)
	}
	// Once the old credential expires, nothing is sent with it.
	now = now.Add(5 * time.Minute)
	s.out.Add(&responseFrame{RequestId: "r2"})
	tick(t, s)
	if _, submits, _ := c.counts(); submits != 1 || len(s.out.Pending()) != 1 {
		t.Fatal("expired credential used")
	}
}

func TestQueuedEventPreventsStaleDrop(t *testing.T) {
	s := testSession(t)
	now := s.pv.now()
	withCredential(s, "c", false)
	gen := s.stream.Start(now.Add(-16*time.Second), "always")
	s.stream.Event(now.Add(-16*time.Second), gen, &aiserverv1.WatchGrokBotUserComputerRequestsEvent{
		Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Connected{Connected: &aiserverv1.WatchGrokBotUserComputerConnected{}},
	})
	s.events <- watched{generation: gen, event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent{
		Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Heartbeat{Heartbeat: &aiserverv1.WatchGrokBotUserComputerHeartbeat{}},
	}}
	s.onTick()
	if !s.stream.Alive() {
		t.Fatal("live stream dropped while its event was queued")
	}
}

// A worker that never finishes gets shutdownGrace-finalFlushGrace; the final
// submit still runs, within shutdownGrace. The budgets expire when the test
// says so, not in real time (T5).
func TestShutdownFlushesDespiteSlowWorker(t *testing.T) {
	var now time.Time
	c := &stubCloud{}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)
	occupy(t, s, "stuck", 1) // never finishes
	s.out.Add(closeFrame("done"))
	var budgets []time.Duration
	s.withTimeout = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		budgets = append(budgets, d)
		ctx, cancel := context.WithCancel(parent)
		if d < shutdownGrace {
			cancel() // the workers' budget is used up
		}
		return ctx, cancel
	}
	start := time.Now()
	s.shutdown()
	if _, submits, _ := c.counts(); submits != 1 || len(s.out.Pending()) != 0 {
		t.Fatal("final submit skipped", submits)
	}
	assert.Equal(t, []time.Duration{shutdownGrace, shutdownGrace - finalFlushGrace}, budgets)
	assert.Less(t, time.Since(start), time.Second)
}

// A submit still in flight when the workers' budget runs out does not stop
// the final submit (C2-2): like the desktop, which gives up waiting for the
// flush in progress, the frames not in flight go out, here the stream close
// of a canceled request and the "stopped serving" answer of a backlogged one;
// the frames in flight are not sent again.
func TestShutdownSubmitsRestWhileSubmitInFlight(t *testing.T) {
	var now time.Time
	c := &stubCloud{holdFirst: make(chan struct{}), held: make(chan struct{})}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)
	s.out.Add(closeFrame("first"))
	s.startSubmit() // hangs on the slow link
	require.True(t, s.out.Submitting())
	<-c.held
	s.out.Add(closeFrame("done"))
	occupy(t, s, "busy", admission.MaxActive)
	s.accept(pinnedExec("queued"), false)
	require.Equal(t, 1, s.admit.Backlog())
	s.withTimeout = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancel(parent)
		if d < shutdownGrace {
			cancel() // the workers' budget is used up
		}
		return ctx, cancel
	}
	s.shutdown()

	c.mu.Lock()
	sent := c.submitted
	c.mu.Unlock()
	require.Len(t, sent, 2)
	got := pinnedFrames(sent, "done")
	require.Len(t, got, 1)
	assert.NotNil(t, pinnedControl(t, got[0]).GetStreamClose())
	got = pinnedFrames(sent, "queued")
	require.Len(t, got, 1)
	assert.Equal(t, admission.ErrStopped.Error(), pinnedControl(t, got[0]).GetThrow().GetError())
	require.Len(t, s.out.Pending(), 1, "the frame in flight stays until its submit settles")
	assert.Equal(t, "first", s.out.Pending()[0].RequestId)

	close(c.holdFirst)
	r := <-s.submitted
	require.NoError(t, r.err)
	c.mu.Lock()
	defer c.mu.Unlock()
	assert.Len(t, pinnedFrames(c.submitted, "first"), 1)
	assert.Len(t, c.submitted, 3)
}

// A full batch that collects while shutdown waits for the workers goes out
// in the background (C3-1). When the workers' budget runs out with its
// answer still on the way, the server may have taken the frames: the final
// submit does not send them again, and each frame reaches the server once.
func TestShutdownDoesNotResendBatchCutShortByWorkersBudget(t *testing.T) {
	var now time.Time
	c := &stubCloud{holdFirst: make(chan struct{}), held: make(chan struct{})}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)
	occupy(t, s, "stuck", 1) // the workers' budget runs out
	for i := range replies.SubmitBatch - 1 {
		s.out.Add(closeFrame(fmt.Sprint("r", i)))
	}
	s.replies <- closeFrame("last") // the 32nd: a full batch
	s.withTimeout = func(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
		if d < shutdownGrace {
			return context.WithTimeout(parent, 300*time.Millisecond)
		}
		return context.WithTimeout(parent, 3*time.Second)
	}
	s.shutdown()
	select {
	case <-c.held:
	default:
		t.Fatal("the full batch was not submitted while waiting for workers")
	}

	close(c.holdFirst) // the late answer
	select {
	case r := <-s.submitted:
		require.NoError(t, r.err)
	case <-time.After(5 * time.Second):
		// The batch was not submitted in the background: the server still
		// takes the frames of the request the client gave up on.
		assert.Eventually(t, func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.submits >= 2
		}, 5*time.Second, 10*time.Millisecond)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]int{}
	for _, f := range c.submitted {
		seen[f.RequestId]++
	}
	assert.Len(t, seen, replies.SubmitBatch)
	for id, n := range seen {
		assert.Equal(t, 1, n, "frames of %s submitted %d times", id, n)
	}
}

func TestPollFailureRetriesWithBackoff(t *testing.T) {
	var now time.Time
	c := &stubCloud{failPoll: true}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)
	s.stream.Start(now, "always")
	s.poll.Request()
	tick(t, s)
	tick(t, s)
	if _, _, polls := c.counts(); polls != 1 || !s.poll.Needed() {
		t.Fatal("failed poll not rescheduled with backoff", polls)
	}
	now = now.Add(time.Second)
	tick(t, s)
	if _, _, polls := c.counts(); polls != 2 {
		t.Fatal("failed poll not retried", polls)
	}
}

// With the stream alive, a notify or a heartbeat with pending requests polls
// at once after failed polls, not when their backoff (up to 60 s) runs out,
// like the desktop, which has no poll backoff (C2-1). Ticks without such an
// event still wait for the backoff, so failing polls follow the events.
func TestWorkEventPollsDespiteBackoff(t *testing.T) {
	var now time.Time
	c := &stubCloud{failPoll: true}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)
	s.stream.Start(now, "always")
	event := func(ev *aiserverv1.WatchGrokBotUserComputerRequestsEvent) {
		s.onEvent(watched{generation: s.stream.Generation(), event: ev})
	}
	heartbeat := func(pending uint32) {
		event(&aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Heartbeat{
			Heartbeat: &aiserverv1.WatchGrokBotUserComputerHeartbeat{PendingRequestCount: pending},
		}})
	}
	polls := func() int {
		_, _, n := c.counts()
		return n
	}
	event(&aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Connected{
		Connected: &aiserverv1.WatchGrokBotUserComputerConnected{},
	}})
	// tickAt advances the clock, keeps the stream alive with a heartbeat that
	// says nothing waits, and ticks.
	tickAt := func(d time.Duration) {
		now = now.Add(d)
		heartbeat(0)
		require.NoError(t, tick(t, s))
	}
	require.NoError(t, tick(t, s))
	for i, wait := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 32 * time.Second} {
		require.Equal(t, i+1, polls())
		tickAt(wait - time.Second)
		require.Equal(t, i+1, polls(), "retry before the backoff")
		tickAt(time.Second)
	}
	require.Equal(t, 7, polls()) // the next retry waits 60 s

	tickAt(time.Second)
	require.Equal(t, 7, polls())
	heartbeat(1)
	require.NoError(t, tick(t, s))
	assert.Equal(t, 8, polls(), "heartbeat with pending requests waited for the backoff")
	tickAt(10 * time.Second)
	assert.Equal(t, 8, polls(), "a retry without an event did not wait for the backoff")

	c.mu.Lock()
	c.failPoll = false
	c.mu.Unlock()
	event(&aiserverv1.WatchGrokBotUserComputerRequestsEvent{Event: &aiserverv1.WatchGrokBotUserComputerRequestsEvent_Notify{
		Notify: &aiserverv1.WatchGrokBotUserComputerNotify{},
	}})
	require.NoError(t, tick(t, s))
	assert.Equal(t, 9, polls(), "notify waited for the backoff")
	assert.True(t, s.stream.Alive())
}

func TestServerAuthoritativeStandingChangeKeepsWatch(t *testing.T) {
	var now time.Time
	s := stubSession(t, &stubCloud{}, &now)
	standing := entity.ModeAlways
	s.pv.Standing = func() entity.Mode { return standing }
	withCredential(s, "c", true)
	s.startWatch()
	gen := s.stream.Generation()
	standing = entity.ModeAsk
	s.onTick()
	if s.stream.Generation() != gen {
		t.Fatal("reconnected for a standing the server ignores")
	}
	withCredential(s, "c", false)
	s.onTick()
	if s.stream.Generation() == gen {
		t.Fatal("standing change not announced")
	}
}

func TestSubmitRunsOffTheRunGoroutine(t *testing.T) {
	var now time.Time
	c := &stubCloud{block: make(chan struct{})}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)
	s.out.Add(closeFrame("a"))
	start := time.Now()
	s.startSubmit()
	if !s.out.Submitting() || time.Since(start) > time.Second {
		t.Fatal("submit blocked the caller")
	}
	close(c.block)
	if err := settle(t, s); err != nil {
		t.Fatal(err)
	}
	if _, submits, _ := c.counts(); submits != 1 || len(s.out.Pending()) != 0 {
		t.Fatal("not submitted", submits)
	}
}

func TestStaleSubmitResultKeepsNewCredential(t *testing.T) {
	var now time.Time
	c := &stubCloud{block: make(chan struct{}), unauthenticated: true}
	s := stubSession(t, c, &now)
	withCredential(s, "old", false)
	s.out.Add(closeFrame("a"))
	s.startSubmit()
	withCredential(s, "new", false)
	close(c.block)
	s.onSubmitted(<-s.submitted)
	if s.cred.Current().Credential != "new" || len(s.out.Pending()) != 1 {
		t.Fatal("rejection of the old credential dropped the new one")
	}
}

// The acks of a poll that returned requests go out with the next poll right
// away, not on the next tick, like the desktop's poll loop.
func TestPolledRequestsAcknowledgedRightAway(t *testing.T) {
	var now time.Time
	c := &stubCloud{requests: []*aiserverv1.GrokBotUserComputerQueuedRequest{{Id: "q1", Frame: execFrame("r1")}}}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)
	s.stream.Start(now, "always")
	s.poll.Request()
	if err := tick(t, s); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pollAcks) != 2 || len(c.pollAcks[1]) != 1 || c.pollAcks[1][0] != "q1" {
		t.Fatal("acks not sent right away", c.pollAcks)
	}
	if _, ok := s.admit.Tasks()["r1"]; !ok {
		t.Fatal("polled request not accepted")
	}
}

// On shutdown the acks of the last poll are delivered, so that the requests
// do not run again after a restart (bug B5); the poll fetches at most one
// request, which is neither started nor acknowledged.
func TestShutdownAcknowledgesPolledRequests(t *testing.T) {
	var now time.Time
	c := &stubCloud{}
	s := stubSession(t, c, &now)
	s.admit.Polled([]*aiserverv1.GrokBotUserComputerQueuedRequest{{Id: "q1", Frame: execFrame("r1")}})
	<-s.pv.ackOnShutdown("c", s.admit.Acks())
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pollAcks) != 1 || len(c.pollAcks[0]) != 1 || c.pollAcks[0][0] != "q1" || c.pollLimits[0] != 1 {
		t.Fatal(c.pollAcks, c.pollLimits)
	}
	<-s.pv.ackOnShutdown("", []string{"q2"}) // no credential: nothing to send
	<-s.pv.ackOnShutdown("c", nil)
	if c.polls != 1 {
		t.Fatal("polled without acks or credential")
	}
}

// A worker blocked on a full reply queue notices the cancellation of its
// request instead of waiting for room (bug B8).
func TestCanceledWorkerNotBlockedByFullQueue(t *testing.T) {
	s := testSession(t)
	withCredential(s, "c", false)
	s.accept(execFrame("flood"), false)
	require.Eventually(t, func() bool { return len(s.replies) == cap(s.replies) }, 5*time.Second, 10*time.Millisecond)
	s.cancel("flood")
	select {
	case id := <-s.done:
		assert.Equal(t, "flood", id)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled worker still blocked on the full queue")
	}
}

// A rejected credential is removed from the cache only if the cache still
// holds it, not a newer one another process saved meanwhile (bug B9).
func TestRejectKeepsNewerCachedCredential(t *testing.T) {
	s := testSession(t)
	withCredential(s, "old", false)
	newer := entity.Credential{Credential: "newer", ExpiresAtMS: s.pv.now().Add(time.Hour).UnixMilli()}
	require.NoError(t, s.pv.Store.Save("computer", newer))
	s.invalidate("", connect.NewError(connect.CodeUnauthenticated, errors.New("expired")))
	var cached entity.Credential
	require.NoError(t, s.pv.Store.Load("computer", &cached))
	assert.Equal(t, "newer", cached.Credential)

	withCredential(s, "newer", false)
	s.invalidate("", connect.NewError(connect.CodeUnauthenticated, errors.New("expired")))
	require.NoError(t, s.pv.Store.Load("computer", &cached))
	assert.Empty(t, cached.Credential)
}

// N3: the user learns once that Grok Bot manages permissions, and again
// when that changes, since --permission has no effect meanwhile.
func TestAuthorityNotice(t *testing.T) {
	s := testSession(t)
	cred := func(server bool) entity.Credential {
		return entity.Credential{Credential: "c", ServerAuthoritative: server}
	}
	assert.Empty(t, s.authorityNotice(entity.Credential{}), "no credential yet")
	assert.Empty(t, s.authorityNotice(cred(false)), "local permissions, as expected")
	assert.Equal(t, "Grok Bot manages permissions for this computer: --permission always has no effect; "+
		"actions run as Grok Bot approves them, and its settings show its own mode", s.authorityNotice(cred(true)))
	assert.Empty(t, s.authorityNotice(cred(true)), "said once")
	assert.Equal(t, "Grok Bot no longer manages permissions for this computer: --permission always applies",
		s.authorityNotice(cred(false)))

	s = testSession(t)
	assert.NotEmpty(t, s.authorityNotice(cred(true)), "server-authoritative from the start")
}
