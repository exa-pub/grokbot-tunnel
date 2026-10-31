// Package execstream runs one exec request as a stream, like the desktop's
// exec manager (anchor: "No handler found for server message of type"): it
// assigns the exec ID, sends heartbeats every 3 s, numbers and times the
// results, keeps each result within 8 MiB and ends the stream with a throw or
// a streamClose.
package execstream

import (
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
)

// MaxJSON is the size limit of a single exec result message.
const MaxJSON = 8 << 20

const heartbeatInterval = 3 * time.Second

// ErrUnsupported matches the desktop daemon's message for unknown requests.
var ErrUnsupported = errors.New("unsupported exec request: No handler found for server message of type undefined")

// Send delivers one result message of a request.
type Send func(*agentv1.ExecClientMessage) error

// Manager assigns exec IDs and runs exec streams.
type Manager struct {
	next atomic.Uint32
}

// Run assigns msg an exec ID and runs exec with a Send for its results,
// heartbeats while it runs and the final frame after it returns: a
// streamClose on success, a throw with cwdState(msg) on failure, and both
// for ErrUnsupported. It returns the first error of sink.
func (m *Manager) Run(
	requestID string, msg *agentv1.ExecServerMessage, sink frames.Sink,
	exec func(Send) error, cwdState func(*agentv1.ExecServerMessage) *string,
) error {
	msg.Id = m.next.Add(1)
	s := &stream{requestID: requestID, msg: msg, start: time.Now(), sink: sink}
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.heartbeats(stop)
	}()
	err := exec(s.send)
	close(stop)
	<-stopped
	var cwd *string
	if err != nil && !errors.Is(err, ErrUnsupported) {
		cwd = cwdState(msg)
	}
	return s.finish(err, cwd)
}

// stream serializes the messages of one exec request: results, heartbeats
// and the final throw or close.
type stream struct {
	mu        sync.Mutex
	requestID string
	msg       *agentv1.ExecServerMessage
	start     time.Time
	sink      frames.Sink
	err       error // first delivery failure; nothing is sent after it
}

// emit delivers a frame; s.mu must be held.
func (s *stream) emit(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
	if s.err == nil {
		s.err = s.sink(f)
	}
	return s.err
}

// control sends a control message; s.mu must be held.
func (s *stream) control(msg *agentv1.ExecClientControlMessage, cwd *string) error {
	return frames.SendControl(s.requestID, msg, cwd, s.emit)
}

// send is the Send of the request's handler.
func (s *stream) send(result *agentv1.ExecClientMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result.Id = s.msg.Id
	result.ExecId = s.msg.ExecId
	result.LocalExecutionTimeMs = elapsedMs(s.start)
	b, err := Marshal(result)
	if err != nil {
		return err
	}
	return s.emit(&aiserverv1.GrokBotUserComputerResponseFrame{
		RequestId: s.requestID,
		Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{
			Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: string(b)},
		},
	})
}

// heartbeats tells the server the request is alive until stop is closed.
func (s *stream) heartbeats(stop <-chan struct{}) {
	tick := time.NewTicker(heartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			s.mu.Lock()
			err := s.control(frames.Heartbeat(s.msg.Id), nil)
			s.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// finish ends the stream with a throw for err, or a close on success.
func (s *stream) finish(err error, cwd *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if errors.Is(err, ErrUnsupported) {
		// Like the desktop, throw and then still close the stream.
		if err := s.control(frames.Throw(s.msg.Id, err), nil); err != nil {
			return err
		}
		err = nil
	}
	if err != nil {
		return s.control(frames.Throw(s.msg.Id, err), cwd)
	}
	return s.control(frames.Close(s.msg.Id), nil)
}

func elapsedMs(start time.Time) *int32 {
	ms := min(time.Since(start).Milliseconds(), math.MaxInt32)
	return proto.Int32(int32(ms)) //nolint:gosec // clamped above
}

// Marshal encodes result, truncating read content until the JSON fits in
// MaxJSON. Other results that are too large fail.
func Marshal(result *agentv1.ExecClientMessage) ([]byte, error) {
	errTooLarge := errors.New("result exceeds 8 MiB")
	for {
		b, err := protojson.Marshal(result)
		if err != nil || len(b) <= MaxJSON {
			return b, err
		}
		read := result.GetReadResult().GetSuccess()
		content, ok := read.GetOutput().(*agentv1.ReadSuccess_Content)
		if !ok || content.Content == "" {
			return nil, errTooLarge
		}
		// Escaping only grows the text, so dropping the excess bytes of the
		// encoding from the raw content converges quickly.
		content.Content = textcodec.TruncateUTF8(content.Content, max(0, len(content.Content)-max(1, len(b)-MaxJSON)))
		read.Truncated = true
	}
}
