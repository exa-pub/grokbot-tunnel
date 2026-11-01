package computer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
)

// heartbeatFrame returns the heartbeat control frame of request id.
func heartbeatFrame(t *testing.T, id string) *responseFrame {
	t.Helper()
	var f *responseFrame
	require.NoError(t, frames.SendControl(id, frames.Heartbeat(1), nil, func(r *responseFrame) error { f = r; return nil }))
	return f
}

// fill fills the reply queue of s, which no loop drains.
func fill(t *testing.T, s *session) {
	t.Helper()
	for len(s.replies) < cap(s.replies) {
		require.NoError(t, s.sink(closeFrame("filler")))
	}
}

// L1: once its request is canceled, a heartbeat (an intermediate control
// frame) does not wait for room in a full queue; it is dropped with an error
// that matches frames.ErrDropped.
func TestTaskSinkDropsHeartbeatOnCancel(t *testing.T) {
	s := testSession(t)
	fill(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	sink := s.taskSink(ctx)
	heartbeat := heartbeatFrame(t, "r")
	done := make(chan error, 1)
	go func() { done <- sink(heartbeat) }()
	select {
	case err := <-done:
		t.Fatalf("heartbeat returned before cancellation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, frames.ErrDropped)
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("canceled heartbeat still waits for room")
	}
}

// L1: the frame that ends a canceled request still waits for room and is
// delivered.
func TestTaskSinkDeliversClosingFrameAfterCancel(t *testing.T) {
	s := testSession(t)
	fill(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- s.taskSink(ctx)(closeFrame("r")) }()
	select {
	case err := <-done:
		t.Fatalf("closing frame not delivered: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	<-s.replies
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("closing frame not delivered after room appeared")
	}
	// A canceled request's intermediate frame still goes out while there is
	// room.
	<-s.replies
	require.NoError(t, s.taskSink(ctx)(heartbeatFrame(t, "r")))
}
