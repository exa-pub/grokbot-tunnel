package replies

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

func chunk(id string, seq uint32, last bool) *Frame {
	return &Frame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_File{
		File: &aiserverv1.GrokBotUserComputerFile{Data: []byte("x"), Seq: seq, Last: last},
	}}
}

func shellExit(id string) *Frame {
	return &Frame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{
		Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: `{"shellStream":{"exit":{"code":0}}}`},
	}}
}

// failRound fails the submits of a round.
func failRound(t *testing.T, q *Queue, now time.Time) {
	t.Helper()
	for range SubmitAttempts {
		batch := q.Next(now)
		require.NotNil(t, batch)
		q.Submitted(now, batch, 0, false, func(string) bool { return false })
		now = now.Add(retryStep)
	}
}

// The bookkeeping of failed rounds does not outlive the frames and
// downloads it is about: a canceled closing frame leaves closingSince, and a
// download replaced by ErrPartialFile leaves partial with its last chunk.
func TestBookkeepingIsReleased(t *testing.T) {
	var q Queue
	now := time.Unix(1000, 0)
	q.Add(shellExit("sh"))
	q.Add(chunk("dl", 0, false))
	failRound(t, &q, now)
	require.Len(t, q.closingSince, 1)
	require.True(t, q.partial["dl"])

	q.Cancel("sh") // drops the kept shell exit
	require.Len(t, q.pending, 1)
	assert.Empty(t, q.closingSince, "closingSince kept a dropped frame")

	assert.False(t, q.Take(chunk("dl", 1, false), false))
	assert.True(t, q.partial["dl"], "partial forgotten before the download ended")
	assert.False(t, q.Take(chunk("dl", 2, true), false))
	assert.Empty(t, q.partial, "partial kept after the download ended")
}

// A last chunk that a cancel replaces with an error leaves closingSince.
func TestCancelReleasesReplacedChunk(t *testing.T) {
	var q Queue
	now := time.Unix(1000, 0)
	whole := chunk("dl", 0, true)
	q.Add(whole)
	failRound(t, &q, now)
	require.Contains(t, q.closingSince, whole)
	q.Cancel("dl")
	require.Len(t, q.pending, 1)
	assert.NotSame(t, whole, q.pending[0])
	assert.Empty(t, q.closingSince)
}
