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
// "stuck" ignore the cancellation until the test ends.
type testHandler struct{ end <-chan struct{} }

func (h testHandler) Handle(ctx context.Context, f *requestFrame, _ bool, sink frames.Sink) error {
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
	block                  chan struct{}                                  // holds submits until closed
	unauthenticated        bool                                           // submits are rejected
	rejectWatch            bool                                           // watches are rejected
	connect                bool                                           // watches report connected
	blockIssue             chan struct{}                                  // holds issues until closed
	requests               []*aiserverv1.GrokBotUserComputerQueuedRequest // returned by the next poll
	pollAcks               [][]string                                     // ack IDs of every poll
	pollLimits             []uint32
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
	return connect.NewResponse(&aiserverv1.IssueGrokBotUserComputerCredentialResponse{
		Credential: fmt.Sprint(c.issues), ExpiresAtMs: time.Now().Add(c.ttl).UnixMilli(),
	}), nil
}

func (c *stubCloud) SubmitGrokBotUserComputerResponses(
	_ context.Context, r *connect.Request[aiserverv1.SubmitGrokBotUserComputerResponsesRequest],
) (*connect.Response[aiserverv1.SubmitGrokBotUserComputerResponsesResponse], error) {
	if c.block != nil {
		<-c.block
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.submits++
	if c.unauthenticated {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("expired"))
	}
	if c.failSubmit {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("down"))
	}
	c.submitted = append(c.submitted, r.Msg.Frames...)
	return connect.NewResponse(&aiserverv1.SubmitGrokBotUserComputerResponsesResponse{AcceptedCount: uint32(len(r.Msg.Frames))}), nil //nolint:gosec // small
}

func (c *stubCloud) PollGrokBotUserComputerRequests(
	_ context.Context, r *connect.Request[aiserverv1.PollGrokBotUserComputerRequestsRequest],
) (*connect.Response[aiserverv1.PollGrokBotUserComputerRequestsResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.polls++
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

func TestShutdownFlushesDespiteSlowWorker(t *testing.T) {
	if testing.Short() {
		t.Skip("waits for the worker grace period")
	}
	var now time.Time
	c := &stubCloud{}
	s := stubSession(t, c, &now)
	withCredential(s, "c", false)
	occupy(t, s, "stuck", 1) // never finishes
	s.out.Add(closeFrame("done"))
	start := time.Now()
	s.shutdown()
	if _, submits, _ := c.counts(); submits != 1 || len(s.out.Pending()) != 0 {
		t.Fatal("final submit skipped", submits)
	}
	if d := time.Since(start); d > shutdownGrace {
		t.Fatal("shutdown overran its grace", d)
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
