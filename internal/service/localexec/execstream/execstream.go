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

var heartbeatInterval = 3 * time.Second // a variable for tests

// ErrUnsupported matches every *UnsupportedError.
var ErrUnsupported = errors.New("no handler for the exec request")

// UnsupportedError fails an exec request that no tool handles, in the
// desktop's words (GGt, or "No handler found for server message of type").
type UnsupportedError struct {
	// Case is the case of the server message as the desktop names it, such
	// as "writeArgs"; empty if the message has none the desktop knows.
	Case string
}

// unsupportedTexts are the desktop's texts for cases it deliberately has no
// handler for (GGt).
var unsupportedTexts = map[string]string{
	"computerUseArgs": "Computer use is not enabled for this environment, " +
		"so the Computer tool and the computerUse subagent cannot run here. " +
		"Do not retry computer actions; verify with non-GUI methods (automated tests, shell commands) instead. " +
		"(No computer-use handler is registered on this exec daemon.)",
	"recordScreenArgs": "Screen recording is not enabled for this environment, so recordings cannot be captured here. " +
		"Do not retry; skip the recording and present other evidence instead. " +
		"(No record-screen handler is registered on this exec daemon.)",
}

func (e *UnsupportedError) Error() string {
	if text, ok := unsupportedTexts[e.Case]; ok {
		return text
	}
	c := e.Case
	if c == "" {
		c = "undefined" // JavaScript's rendering of the missing case
	}
	return "No handler found for server message of type " + c
}

// Is makes errors.Is(err, ErrUnsupported) match.
func (e *UnsupportedError) Is(target error) bool { return target == ErrUnsupported }

// Send delivers one result message of a request.
type Send func(*agentv1.ExecClientMessage) error

// Manager assigns exec IDs and runs exec streams.
type Manager struct {
	next atomic.Uint32
}

// Run assigns msg an exec ID and runs exec with a Send for its results,
// heartbeats while it runs and the final frame after it returns: a
// streamClose on success, a throw with cwdState(msg) on failure, and both
// for an *UnsupportedError. cwdState sees msg as exec left it. A failure
// that only reports a frame the sink dropped on cancellation
// (frames.ErrDropped) is a success, as on the desktop, whose stream never
// refuses a frame. Results and heartbeats are sent from different
// goroutines, so sink must be safe for concurrent use. Run returns the first
// error of sink other than a drop.
func (m *Manager) Run(
	requestID string, msg *agentv1.ExecServerMessage, sink frames.Sink,
	exec func(Send) error, cwdState func(*agentv1.ExecServerMessage) *string,
) error {
	msg.Id = m.next.Add(1)
	s := &stream{requestID: requestID, id: msg.Id, start: time.Now(), sink: sink}
	stop := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		s.heartbeats(stop)
	}()
	err := exec(s.send)
	close(stop)
	<-stopped
	return s.finish(err, func() *string { return cwdState(msg) })
}

// stream is one exec request: its results, heartbeats and the final throw
// or close.
type stream struct {
	mu        sync.Mutex // serializes results; guards err
	requestID string
	id        uint32
	start     time.Time
	sink      frames.Sink
	err       error // first delivery failure other than a drop; nothing is sent after it
}

// failed returns the delivery failure that stopped the stream, if any.
func (s *stream) failed() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// record keeps err as the stream's failure unless it is a drop on
// cancellation; s.mu must be held.
func (s *stream) record(err error) {
	if err != nil && !errors.Is(err, frames.ErrDropped) && s.err == nil {
		s.err = err
	}
}

// control sends a control message without holding s.mu while the sink
// waits.
func (s *stream) control(msg *agentv1.ExecClientControlMessage, cwd *string) error {
	if err := s.failed(); err != nil {
		return err
	}
	err := frames.SendControl(s.requestID, msg, cwd, s.sink)
	s.mu.Lock()
	s.record(err)
	s.mu.Unlock()
	return err
}

// send is the Send of the request's handler.
func (s *stream) send(result *agentv1.ExecClientMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	// Like the desktop's result serializers, the message carries the exec
	// ID and the time, not the server's execId.
	result.Id = s.id
	result.LocalExecutionTimeMs = elapsedMs(s.start)
	b, err := Marshal(result)
	if err != nil {
		return err
	}
	err = s.sink(&aiserverv1.GrokBotUserComputerResponseFrame{
		RequestId: s.requestID,
		Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{
			Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: string(b)},
		},
	})
	s.record(err)
	return err
}

// heartbeats tells the server the request is alive until stop is closed. A
// heartbeat waits for the sink without holding s.mu, so that neither the
// results nor the end of a canceled request wait behind it.
func (s *stream) heartbeats(stop <-chan struct{}) {
	tick := time.NewTicker(heartbeatInterval)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			if err := s.control(frames.Heartbeat(s.id), nil); err != nil && !errors.Is(err, frames.ErrDropped) {
				return
			}
		}
	}
}

// finish ends the stream with a throw for err, or a close on success. Run
// calls it after the heartbeats stopped, so nothing follows its frame.
func (s *stream) finish(err error, cwdState func() *string) error {
	if err := s.failed(); err != nil {
		return err
	}
	switch {
	case errors.Is(err, frames.ErrDropped):
		err = nil
	case errors.Is(err, ErrUnsupported):
		// Like the desktop, throw without a stack trace and then still
		// close the stream.
		if err := s.control(frames.Throw(s.id, err), cwdState()); err != nil {
			return err
		}
		err = nil
	}
	if err != nil {
		return s.control(frames.ToolThrow(s.id, err), cwdState())
	}
	return s.control(frames.Close(s.id), nil)
}

// elapsedMs is the time since start in milliseconds.
func elapsedMs(start time.Time) *int32 { return proto.Int32(roundMs(time.Since(start))) }

// roundMs rounds d to milliseconds like the desktop's
// Math.round(Math.max(0, ms)).
func roundMs(d time.Duration) int32 {
	ms := min(max(0, d).Round(time.Millisecond).Milliseconds(), math.MaxInt32)
	return int32(ms) //nolint:gosec // clamped above
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
