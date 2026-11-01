package replies_test

import (
	"context"
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

// Like the desktop's MX.take, a request whose frames were accepted moves to
// the end of the serving order, so parallel downloads, one chunk per batch,
// take turns; a failed batch does not rotate, and a new request comes after
// the known ones.
func TestBatchRotatesRequests(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(q *replies.Queue)
		fail  int // the first fail batches are not accepted
		want  string
	}{
		{"two downloads", func(q *replies.Queue) {
			for i := range 3 {
				q.Add(file("a", uint32(i), 1<<20, false)) //nolint:gosec // small
			}
			for i := range 3 {
				q.Add(file("b", uint32(i), 1<<20, false)) //nolint:gosec // small
			}
		}, 0, "ababab"},
		{"failed batch keeps its place", func(q *replies.Queue) {
			q.Add(file("a", 0, 1<<20, false))
			q.Add(file("a", 1, 1<<20, false))
			q.Add(file("b", 0, 1<<20, false))
		}, 2, "aaaba"},
		{"three downloads", func(q *replies.Queue) {
			for _, id := range []string{"a", "b", "c"} {
				for i := range 2 {
					q.Add(file(id, uint32(i), 1<<20, false)) //nolint:gosec // small
				}
			}
		}, 0, "abcabc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var q replies.Queue
			tc.setup(&q)
			got := ""
			for i := 0; len(q.Pending()) > 0; i++ {
				batch := q.Batch()
				require.Len(t, batch, 1)
				got += batch[0].RequestId
				if i < tc.fail {
					q.Accepted(batch, 0)
					continue
				}
				q.Accepted(batch, 1)
			}
			assert.Equal(t, tc.want, got)
		})
	}
	var q replies.Queue
	q.Add(file("a", 0, 1<<20, false))
	q.Add(file("a", 1, 1<<20, false))
	q.Add(file("b", 0, 1<<20, false))
	q.Add(file("b", 1, 1<<20, false))
	q.Accepted(q.Batch(), 1) // a, then b is first
	q.Add(file("c", 0, 1<<20, false))
	got := ""
	for len(q.Pending()) > 0 {
		batch := q.Batch()
		got += batch[0].RequestId
		q.Accepted(batch, 1)
	}
	assert.Equal(t, "bacb", got, "new request served before known ones")
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
	again, failed := q.Submitted(now, batch, 1, false, never)
	assert.True(t, again)
	assert.Nil(t, failed)
	assert.True(t, q.Accepting())
	assert.Len(t, q.Pending(), replies.SubmitBatch)
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

func exit(id string) *replies.Frame {
	return &replies.Frame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{
		Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: `{"shellStream":{"exit":{"code":0}}}`},
	}}
}

func stdout(id string) *replies.Frame {
	return &replies.Frame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{
		Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: `{"shellStream":{"stdout":{"data":"x"}}}`},
	}}
}

// failRound submits batch and fails it SubmitAttempts times, as the session
// would, and returns the Failure of the round.
func failRound(t *testing.T, q *replies.Queue, now *time.Time, canceled func(string) bool) *replies.Failure {
	t.Helper()
	for i := range replies.SubmitAttempts {
		batch := q.Next(*now)
		require.NotNil(t, batch, "attempt %d", i)
		_, failed := q.Submitted(*now, batch, 0, false, canceled)
		if i < replies.SubmitAttempts-1 {
			require.Nil(t, failed)
			assert.Nil(t, q.Next(*now), "retried before the delay")
			*now = now.Add(time.Duration(i+1) * 200 * time.Millisecond)
			continue
		}
		require.NotNil(t, failed)
		return failed
	}
	return nil
}

// After a failed round only frames that close their requests stay, like the
// desktop's KEr: results, shell exits, throw and streamClose, whole files,
// file errors, messages and local tool results; output, keep-alives, file
// chunks and frames of canceled requests are dropped. A download that lost a
// chunk ends with ErrPartialFile, and its later chunks are dropped too.
func TestFailedRoundKeepsClosingFrames(t *testing.T) {
	var q replies.Queue
	now := time.Unix(1000, 0)
	heartbeat := &replies.Frame{RequestId: "hb", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Control{
		Control: &aiserverv1.GrokBotUserComputerControlMessage{MessageJson: `{"heartbeat":{"id":1}}`},
	}}
	tool := &replies.Frame{RequestId: "tool", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_LocalToolResult{
		LocalToolResult: &aiserverv1.GrokBotUserComputerLocalToolResult{Status: "ok"},
	}}
	read := &replies.Frame{RequestId: "read", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{
		Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: `{"readResult":{}}`},
	}}
	for _, f := range []*replies.Frame{
		stdout("sh"), exit("sh"), closeFrame("sh"), heartbeat, keepAlive("ka"), read, tool,
		file("whole", 0, 1, true), file("dl", 0, 1, false), exit("gone"),
	} {
		q.Add(f)
	}
	failed := failRound(t, &q, &now, func(id string) bool { return id == "gone" })
	assert.Equal(t, replies.Failure{Kept: 5, Dropped: 5}, *failed)
	kept := make([]string, 0, len(q.Pending()))
	for _, f := range q.Pending() {
		kept = append(kept, f.RequestId)
	}
	assert.Equal(t, []string{"sh", "sh", "read", "tool", "whole", "dl"}, kept)
	last := q.Pending()[5]
	assert.Equal(t, replies.ErrPartialFile, last.GetFileError().GetError())
	assert.False(t, q.Take(file("dl", 1, 1, true), false), "chunk after the partial-file error queued")
	assert.Len(t, q.Pending(), 6)
}

// Kept closing frames wait 0.5 s, 1 s, 2 s, 4 s, 4 s between rounds and are
// dropped once their budget, counted from their first failed round, is spent.
func TestClosingFramesRetryWithinBudget(t *testing.T) {
	var q replies.Queue
	now := time.Unix(1000, 0)
	q.Add(exit("sh"))
	start := now
	for _, ms := range []int{500, 1000, 2000, 4000, 4000} {
		failed := failRound(t, &q, &now, never)
		require.Equal(t, replies.Failure{Kept: 1}, *failed)
		wait := time.Duration(ms) * time.Millisecond
		assert.Nil(t, q.Next(now.Add(wait-time.Millisecond)), wait)
		now = now.Add(wait)
	}
	now = start.Add(30 * time.Second)
	failed := failRound(t, &q, &now, never)
	assert.Equal(t, replies.Failure{Dropped: 1, Expired: 1}, *failed)
	assert.Empty(t, q.Pending())
	// Only a successful submit resets the closing retry delay.
	q.Add(exit("next"))
	failRound(t, &q, &now, never)
	now = now.Add(4 * time.Second)
	batch := q.Next(now)
	require.NotNil(t, batch)
	again, failed := q.Submitted(now, batch, 1, false, never)
	assert.True(t, again)
	assert.Nil(t, failed)
}

// Within a round a failed submit is retried after a constant 200 ms, like the
// desktop's user-computer-submit.
func TestSubmitRetryStep(t *testing.T) {
	var q replies.Queue
	now := time.Unix(1000, 0)
	q.Add(stdout("a"))
	for i := range replies.SubmitAttempts - 1 {
		batch := q.Next(now)
		require.NotNil(t, batch, i)
		_, failed := q.Submitted(now, batch, 0, false, never)
		require.Nil(t, failed)
		assert.Nil(t, q.Next(now.Add(199*time.Millisecond)), i)
		now = now.Add(200 * time.Millisecond)
	}
	assert.NotNil(t, q.Next(now))
}

// Like the desktop, whose enqueue flushes at once, a new frame ends the pause
// after a round of only closing frames and goes along with them; a
// keep-alive does not.
func TestNewFrameEndsClosingPause(t *testing.T) {
	var q replies.Queue
	now := time.Unix(1000, 0)
	q.Add(exit("sh"))
	failRound(t, &q, &now, never)
	q.Add(keepAlive("ka"))
	assert.Nil(t, q.Next(now), "keep-alive ended the pause")
	q.Add(stdout("new"))
	batch := q.Next(now)
	require.Len(t, batch, 3)
	ids := []string{batch[0].RequestId, batch[1].RequestId, batch[2].RequestId}
	assert.ElementsMatch(t, []string{"sh", "ka", "new"}, ids)
	// A new frame does not cut the retry delay within a round.
	_, failed := q.Submitted(now, batch, 0, false, never)
	require.Nil(t, failed)
	q.Add(stdout("during the round"))
	assert.Nil(t, q.Next(now), "in-round retry delay cut")
	assert.NotNil(t, q.Next(now.Add(200*time.Millisecond)))
}

// While other frames wait behind the kept ones, the next round starts at once.
func TestFailedRoundWithMoreFramesSubmitsAgain(t *testing.T) {
	var q replies.Queue
	now := time.Unix(1000, 0)
	q.Add(exit("a"))
	for range 300 {
		q.Add(stdout("b"))
	}
	failed := failRound(t, &q, &now, never) // a batch holds 256 frames
	assert.Equal(t, replies.Failure{Kept: 1, Dropped: 255}, *failed)
	assert.Len(t, q.Pending(), 1+45)
	assert.NotNil(t, q.Next(now), "remaining frames held back")
}

// A rejected credential drops nothing: the frames wait for a new one.
func TestRejectedSubmitKeepsFrames(t *testing.T) {
	var q replies.Queue
	now := time.Unix(1000, 0)
	q.Add(stdout("a"))
	for range replies.SubmitAttempts + 1 {
		batch := q.Next(now)
		require.NotNil(t, batch)
		again, failed := q.Submitted(now, batch, 0, true, never)
		assert.False(t, again)
		assert.Nil(t, failed)
	}
	assert.Len(t, q.Pending(), 1)
}

// Workers wait while MaxPending frames other than keep-alives are pending.
func TestAcceptingBackpressure(t *testing.T) {
	var q replies.Queue
	for i := range replies.MaxPending {
		q.Add(keepAlive(fmt.Sprint(i)))
	}
	q.Add(closeFrame("x"))
	assert.Equal(t, 1, q.Stuck())
	assert.True(t, q.Accepting())
	for i := range replies.MaxPending - 1 {
		q.Add(client(fmt.Sprint(i)))
	}
	assert.False(t, q.Accepting())
}

// Workers also wait while MaxPendingData bytes of file data are unsent, so
// downloads do not pile up chunks faster than they are submitted.
func TestAcceptingBoundsFileData(t *testing.T) {
	var q replies.Queue
	for i := range replies.MaxPendingData>>20 - 1 {
		q.Add(file("dl", uint32(i), 1<<20, false)) //nolint:gosec // small
	}
	q.Add(file("dl", 99, 1<<20-1, false))
	assert.True(t, q.Accepting())
	q.Add(file("other", 0, 1, false))
	assert.False(t, q.Accepting(), "unsent file data not bounded")
	q.Accepted(q.Batch(), 1)
	assert.True(t, q.Accepting())
}

// A cancel leaves the frames of the submit in flight alone, so that a last
// chunk the server accepts does not get a second final frame (bug B6).
func TestCancelSparesSubmitInFlight(t *testing.T) {
	var q replies.Queue
	now := time.Unix(1000, 0)
	q.Add(file("dl", 0, 1, false))
	q.Add(file("dl", 1, 1, true))
	batch := q.Next(now)
	require.Len(t, batch, 1, "one chunk per request per batch")
	q.Cancel("dl")
	require.Len(t, q.Pending(), 2)
	assert.Same(t, batch[0], q.Pending()[0], "frame in flight replaced")
	assert.Equal(t, context.Canceled.Error(), q.Pending()[1].GetFileError().GetError())
	q.Submitted(now, batch, 1, false, never)
	require.Len(t, q.Pending(), 1)
	assert.NotNil(t, q.Pending()[0].GetFileError())
	// The last chunk itself in flight: accepted, nothing else ends the request.
	var r replies.Queue
	r.Add(file("x", 0, 1, true))
	batch = r.Next(now)
	r.Cancel("x")
	r.Submitted(now, batch, 1, false, never)
	assert.Empty(t, r.Pending())
}
