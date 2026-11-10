package computer

import (
	"context"
	"log"
	"time"

	"connectrpc.com/connect"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/uuid"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/admission"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/credential"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/replies"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/watch"
)

const (
	tickInterval = 100 * time.Millisecond
	pollInterval = 30 * time.Second
)

// channelRetry is how long the watch stays down after the server rejected the
// credential, as the desktop daemon's channelRetry. A variable so tests can
// shorten it.
var channelRetry = 10 * time.Second

// submitted is the outcome of a submit RPC run off the Run goroutine.
type submitted struct {
	credential string
	frames     []*responseFrame
	accepted   int
	err        error
}

// aliveSent is the outcome of a keep-alive submit run off the Run goroutine.
type aliveSent struct {
	credential string
	frames     []*responseFrame
	err        error
}

// polled is the outcome of a poll RPC run off the Run goroutine.
type polled struct {
	credential string
	server     bool // the credential's ServerAuthoritative
	requests   []*aiserverv1.GrokBotUserComputerQueuedRequest
	err        error
}

// issued is the outcome of a credential issue run off the Run goroutine.
type issued struct {
	credential entity.Credential
	err        error
}

// watched is an event or the terminal error of a watch stream.
type watched struct {
	event      *aiserverv1.WatchGrokBotUserComputerRequestsEvent
	err        error
	generation int
}

// session holds the state of Run. All fields are owned by the Run goroutine;
// workers and the RPCs talk to it only through the channels.
type session struct {
	pv *Provider

	// presence identifies this session's watch streams to the server, which
	// forgets the computer's presence when it is withdrawn.
	presence string

	// work is canceled on shutdown and parents watch and task contexts.
	work     context.Context
	stopWork context.CancelFunc
	// life outlives work so canceled tasks can still report their results.
	life     context.Context
	stopLife context.CancelFunc

	replies chan *responseFrame
	done    chan string
	events  chan watched
	// At most one submit, one keep-alive submit, one poll and one issue are
	// in flight; their results come back on these channels, buffered so an
	// abandoned RPC never blocks.
	submitted chan submitted
	aliveSent chan aliveSent
	polled    chan polled
	issued    chan issued

	cred *credential.Manager
	// authorityKnown is set once a watch started with a credential, whose
	// ServerAuthoritative serverAuthority keeps (see authorityNotice).
	authorityKnown, serverAuthority bool
	stream                          *watch.Loop
	watchCancel                     context.CancelFunc
	admit                           *admission.Scheduler
	poll                            admission.Poller
	out                             replies.Queue
	alive                           replies.KeepAlive
	// connected is set once the server first accepted the computer.
	connected bool

	// withTimeout is context.WithTimeout; tests expire shutdown's budgets
	// without waiting for them.
	withTimeout func(context.Context, time.Duration) (context.Context, context.CancelFunc)
}

func newSession(ctx context.Context, pv *Provider) *session {
	s := &session{
		pv:        pv,
		presence:  uuid.New(),
		replies:   make(chan *responseFrame, replies.SubmitBatch),
		done:      make(chan string, admission.MaxActive),
		events:    make(chan watched, 16),
		submitted: make(chan submitted, 1),
		aliveSent: make(chan aliveSent, 1),
		polled:    make(chan polled, 1),
		issued:    make(chan issued, 1),
		cred:      credential.New(pv.cachedCredential(), pv.jitter),
		stream:    watch.New(pv.random),
		admit:     admission.New(),

		withTimeout: context.WithTimeout,
	}
	s.life, s.stopLife = context.WithCancel(context.Background())
	s.work, s.stopWork = context.WithCancel(ctx)
	return s
}

func (s *session) close() {
	if s.watchCancel != nil {
		s.watchCancel()
	}
	s.stopWork()
	s.stopLife()
}

// sink is the handlers' output; it is safe to call from any goroutine.
func (s *session) sink(f *responseFrame) error {
	select {
	case s.replies <- f:
		return nil
	case <-s.life.Done():
		return s.life.Err()
	}
}

// taskSink is sink for a task run under ctx. Only a frame that ends the
// request (frames.Closes) and the exit event of a shell command, which says
// how a canceled command ended, wait for room regardless of ctx. Once ctx is
// canceled, any other frame, heartbeats and other intermediate control
// messages included, is dropped with frames.Dropped instead of waiting, so
// that a worker blocked on a full queue notices the cancellation (bugs B8,
// L1) and still sends the frames that end the request.
func (s *session) taskSink(ctx context.Context) frames.Sink {
	return func(f *responseFrame) error {
		select {
		case s.replies <- f:
			return nil
		default:
		}
		if frames.Closes(f) || frames.ShellExit(f) {
			return s.sink(f)
		}
		select {
		case s.replies <- f:
			return nil
		case <-s.life.Done():
			return s.life.Err()
		case <-ctx.Done():
			return frames.Dropped(ctx.Err())
		}
	}
}

func (s *session) run(ctx context.Context) error {
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	pollTick := time.NewTicker(pollInterval)
	defer pollTick.Stop()
	aliveTick := time.NewTicker(replies.AliveTick)
	defer aliveTick.Stop()
	for {
		var err error
		select {
		case <-ctx.Done():
			s.stop()
			return nil
		case f := <-s.accepting():
			if s.out.Take(f, s.admit.Canceled(f.RequestId)) {
				s.startSubmit()
			}
		case r := <-s.submitted:
			s.onSubmitted(r)
		case r := <-s.aliveSent:
			s.onAliveSent(r)
		case r := <-s.polled:
			s.onPolled(r)
		case r := <-s.issued:
			err = s.onIssued(r)
		case id := <-s.done:
			s.admit.Finish(id)
			s.startJobs()
		case ev := <-s.events:
			s.onEvent(ev)
		case <-aliveTick.C:
			s.keepAlive()
		case <-pollTick.C:
			s.poll.Request()
		case <-tick.C:
			s.onTick()
		}
		if err != nil {
			s.stop()
			return err
		}
	}
}

// accepting is the channel of the workers' replies, or nil while enough are
// pending: the workers wait then.
func (s *session) accepting() chan *responseFrame {
	if s.out.Accepting() {
		return s.replies
	}
	return nil
}

// keepAlive tells the server, in a submit of its own, that requests it has
// not heard of for a while are alive: those that wait for their turn and
// those with frames it has not accepted. Canceled requests are left out;
// without a credential nothing could be delivered.
func (s *session) keepAlive() {
	credential := s.cred.Current().Credential
	if credential == "" {
		return
	}
	now := s.pv.now()
	held, eligible := s.out.Unheard(now)
	for id, started := range s.admit.Tasks() {
		held[id] = true
		if !started {
			eligible[id] = true
		}
	}
	for id := range eligible {
		if s.admit.Canceled(id) {
			delete(eligible, id)
		}
	}
	batch := s.alive.Next(now, held, eligible)
	if batch == nil {
		return
	}
	go func() {
		_, err := s.pv.submitFrames(s.life, replies.AliveTimeout, credential, batch)
		s.aliveSent <- aliveSent{credential: credential, frames: batch, err: err}
	}()
}

// onAliveSent applies the result of a keep-alive submit. Like the desktop,
// only a rejected credential and an exhausted server change anything; a
// keep-alive that failed otherwise is due again on the next tick.
func (s *session) onAliveSent(r aliveSent) {
	code := connect.CodeOf(r.err)
	if r.err != nil && code == connect.CodeUnauthenticated {
		s.invalidate(r.credential, r.err)
	}
	s.alive.Sent(s.pv.now(), r.frames, r.err == nil, r.err != nil && code == connect.CodeResourceExhausted)
}

func (s *session) onTick() {
	usable, issue := s.cred.Ensure(s.pv.now(), s.work.Err() != nil)
	if issue {
		go func() {
			c, err := s.pv.issue(s.work)
			s.issued <- issued{credential: c, err: err}
		}()
	}
	if !usable {
		return
	}
	// Events that queued up while Run was busy prove the stream is alive.
drain:
	for range cap(s.events) {
		select {
		case ev := <-s.events:
			s.onEvent(ev)
		default:
			break drain
		}
	}
	switch s.stream.Tick(s.pv.now(), s.pv.standing(s.cred.Current())) {
	case watch.Dial:
		s.startWatch()
		s.poll.Reset()
	case watch.Redial:
		s.startWatch() // announces the new standing mode
	case watch.Drop:
		if s.watchCancel != nil {
			s.watchCancel()
		}
	case watch.Renew:
		if s.watchCancel != nil {
			s.watchCancel()
		}
		// Like the desktop, which renews its client then: the network may
		// have dropped the pooled connections too.
		s.pv.closeIdle()
	case watch.None:
	}
	s.startSubmit()
	s.startPoll()
}

func (s *session) onEvent(ev watched) {
	var out watch.Outcome
	if ev.event == nil {
		out = s.stream.End(s.pv.now(), ev.generation, ev.err)
	} else {
		out = s.stream.Event(s.pv.now(), ev.generation, ev.event)
	}
	switch {
	case out.Stale:
	case out.Ended:
		s.invalidate("", ev.err)
		log.Printf("watch: %v; reconnecting", ev.err)
	case out.Connected:
		// Like the desktop, poll at once, whatever failed before.
		s.poll.Reset()
		s.cred.Connected()
		log.Printf("connected as %s (%s)", s.pv.label(), s.pv.Machine.MachineID)
		if !s.connected {
			s.connected = true
			if s.pv.OnConnected != nil {
				s.pv.OnConnected()
			}
		}
	case out.Work:
		// Like the desktop, poll at once, even while failed polls back off.
		s.poll.Trigger()
	}
}
