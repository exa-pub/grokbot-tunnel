package replies_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/computer/replies"
)

func file(id string, seq uint32, size int, last bool) *replies.Frame {
	return &replies.Frame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_File{
		File: &aiserverv1.GrokBotUserComputerFile{Data: make([]byte, size), Seq: seq, Last: last},
	}}
}

func client(id string) *replies.Frame {
	return &replies.Frame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{
		Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: "result"},
	}}
}

func closeFrame(id string) *replies.Frame {
	return &replies.Frame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Control{
		Control: &aiserverv1.GrokBotUserComputerControlMessage{MessageJson: `{"streamClose":{"id":3}}`},
	}}
}

func keepAlive(id string) *replies.Frame {
	return &replies.Frame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_MessagesAccepted{
		MessagesAccepted: &aiserverv1.GrokBotUserComputerMessagesAccepted{},
	}}
}

func never(string) bool { return false }

// Invariant 8: requests are interleaved fairly, order within a request is
// kept, a batch holds at most 1 MiB of file data and 256 frames, and file
// transfers do not starve metadata.
func TestBatchFairnessAndBudgets(t *testing.T) {
	var q replies.Queue
	for _, id := range []string{"a", "b"} {
		for i := range 3 {
			q.Add(file(id, uint32(i), 1<<20, false)) //nolint:gosec // small
		}
	}
	batch := q.Batch()
	require.Len(t, batch, 1, "file bytes budget ignored")
	assert.Equal(t, "a", batch[0].RequestId)
	q.Accepted(batch, 1)
	require.Len(t, q.Batch(), 1, "file bytes budget ignored")
	q.Add(closeFrame("c"))
	batch = q.Batch()
	assert.Equal(t, "c", batch[0].RequestId, "metadata starved by file transfer")
	data := 0
	for _, f := range batch {
		data += len(f.GetFile().GetData())
	}
	assert.LessOrEqual(t, data, 1<<20)
	for i := range 300 {
		q.Add(keepAlive(fmt.Sprint(i)))
	}
	assert.LessOrEqual(t, len(q.Batch()), 256, "frame budget ignored")
}

func TestBatchKeepsOrderWithinRequest(t *testing.T) {
	var q replies.Queue
	for i := range 7 {
		q.Add(file("a", uint32(i), 1, false)) //nolint:gosec // small
	}
	var seqs []uint32
	for len(q.Pending()) > 0 {
		batch := q.Batch()
		require.Len(t, batch, 1, "one chunk per request per batch")
		seqs = append(seqs, batch[0].GetFile().GetSeq())
		q.Accepted(batch, 1)
	}
	assert.Equal(t, []uint32{0, 1, 2, 3, 4, 5, 6}, seqs)
}

// Replies of a canceled request are dropped unless they end its stream; the
// last chunk of a canceled transfer becomes an error.
func TestTakeCanceled(t *testing.T) {
	var q replies.Queue
	assert.False(t, q.Take(client("x"), true))
	assert.False(t, q.Take(file("x", 0, 1, false), true))
	assert.Empty(t, q.Pending())
	q.Take(file("x", 1, 1, true), true)
	require.Len(t, q.Pending(), 1)
	assert.Equal(t, context.Canceled.Error(), q.Pending()[0].GetFileError().GetError())
	q.Take(closeFrame("x"), true)
	assert.Len(t, q.Pending(), 2)
}

func TestTakeReportsFullBatch(t *testing.T) {
	var q replies.Queue
	for i := range replies.SubmitBatch - 1 {
		assert.False(t, q.Take(client(fmt.Sprint(i)), false))
	}
	assert.True(t, q.Take(client("last"), false))
}

// Invariant 5: a cancel discards the undelivered frames of the request but
// keeps the frames that close its stream, and turns its pending last chunk
// into an error.
func TestCancel(t *testing.T) {
	var q replies.Queue
	q.Add(client("fin"))
	q.Add(closeFrame("fin"))
	q.Add(file("late", 0, 1, false))
	q.Add(file("late", 1, 1, true))
	q.Add(client("other"))
	q.Cancel("fin")
	q.Cancel("late")
	require.Len(t, q.Pending(), 3)
	assert.NotNil(t, q.Pending()[0].GetControl(), "cancel purged the stream close")
	assert.Equal(t, context.Canceled.Error(), q.Pending()[1].GetFileError().GetError())
	assert.Equal(t, "other", q.Pending()[2].RequestId)
}

// One submit in flight; workers wait while a full batch is pending behind it.
func TestNextAndAccepting(t *testing.T) {
	var q replies.Queue
	now := time.Unix(1000, 0)
	assert.Nil(t, q.Next(now), "nothing pending")
	q.Add(closeFrame("a"))
	batch := q.Next(now)
	require.Len(t, batch, 1)
	assert.True(t, q.Submitting())
	for i := range replies.SubmitBatch {
		q.Add(closeFrame(fmt.Sprint(i)))
	}
	assert.Nil(t, q.Next(now), "second submit in flight")
	assert.False(t, q.Accepting(), "replies not throttled while a full batch waits")
	again, exhausted := q.Submitted(now, batch, 1, true)
	assert.True(t, again)
	assert.False(t, exhausted)
	assert.True(t, q.Accepting())
	assert.Len(t, q.Pending(), replies.SubmitBatch)
}

// Failed submits wait 200 ms, then 400 ms; the third ends the round, after
// which the next submit may start at once.
func TestSubmittedRetries(t *testing.T) {
	var q replies.Queue
	now := time.Unix(1000, 0)
	q.Add(closeFrame("a"))
	for i, wait := range []time.Duration{200 * time.Millisecond, 400 * time.Millisecond} {
		batch := q.Next(now)
		require.NotNil(t, batch, i)
		again, exhausted := q.Submitted(now, batch, 0, true)
		require.False(t, again)
		require.False(t, exhausted)
		assert.Nil(t, q.Next(now.Add(wait-time.Millisecond)), "retried before the delay")
		now = now.Add(wait)
	}
	batch := q.Next(now)
	_, exhausted := q.Submitted(now, batch, 0, true)
	assert.True(t, exhausted)
	assert.NotNil(t, q.Next(now), "round restarts at once")
	// Without a credential a failure ends the round right away.
	_, exhausted = q.Submitted(now, batch, 0, false)
	assert.True(t, exhausted)
}

// Keep-alives alone never fill the backlog; other frames do.
func TestOverflow(t *testing.T) {
	var q replies.Queue
	fail := errors.New("down")
	for i := range replies.MaxPending {
		q.Add(keepAlive(fmt.Sprint(i)))
	}
	q.Add(closeFrame("x"))
	assert.Equal(t, 1, q.Stuck())
	require.NoError(t, q.Overflow(fail))
	assert.True(t, q.Accepting())
	for i := range replies.MaxPending - 1 {
		q.Add(client(fmt.Sprint(i)))
	}
	err := q.Overflow(fail)
	require.ErrorIs(t, err, replies.ErrBacklogFull)
	require.ErrorIs(t, err, fail)
	assert.False(t, q.Accepting())
}

// Invariant 11, ≠ desktop: keep-alives go to started tasks and to requests
// with unsent frames, not to waiting or canceled ones, nor once a stream is
// closed or a keep-alive is pending; the desktop keeps alive the backlog,
// unsent replies and the submit in flight, but not running requests.
func TestKeepAlive(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tasks    map[string]bool
		pending  []*replies.Frame
		canceled bool
		want     int // keep-alives for request r afterwards
	}{
		{"started task", map[string]bool{"r": true}, nil, false, 1},
		{"waiting in backlog", map[string]bool{"r": false}, nil, false, 0},
		{"unsent reply of finished request", nil, []*replies.Frame{client("r")}, false, 1},
		{"unsent reply of started task", map[string]bool{"r": true}, []*replies.Frame{client("r")}, false, 1},
		{"stream close pending", nil, []*replies.Frame{closeFrame("r")}, false, 0},
		{"canceled", map[string]bool{"r": true}, nil, true, 0},
		{"keep-alive pending", map[string]bool{"r": true}, []*replies.Frame{keepAlive("r")}, false, 1},
	} {
		var q replies.Queue
		for _, f := range tc.pending {
			q.Add(f)
		}
		q.KeepAlive(tc.tasks, func(string) bool { return tc.canceled })
		n := 0
		for _, f := range q.Pending() {
			if f.RequestId == "r" && f.GetMessagesAccepted() != nil {
				n++
			}
		}
		assert.Equal(t, tc.want, n, tc.name)
	}
}

// Repeated keep-alive rounds add at most one per request.
func TestKeepAliveIsBounded(t *testing.T) {
	var q replies.Queue
	tasks := map[string]bool{"queued": false}
	for i := range 72 {
		tasks[fmt.Sprint(i)] = true
	}
	q.Add(closeFrame("closed"))
	for range 10 {
		q.KeepAlive(tasks, never)
	}
	alive := map[string]int{}
	for _, f := range q.Pending() {
		if f.GetMessagesAccepted() != nil {
			alive[f.RequestId]++
		}
	}
	assert.Len(t, alive, 72)
	for id, n := range alive {
		assert.Equal(t, 1, n, id)
	}
	assert.Equal(t, 1, q.Stuck())
}
