package replies

import (
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

// Retry budgets of closing frames after a failed submit round, like the
// desktop's KEr: how long a frame that ends its request is retried.
const (
	execBudget     = 30 * time.Second  // results, shell exits, throw, streamClose
	fileBudget     = 120 * time.Second // whole files and file errors
	messagesBudget = 180 * time.Second // messages and local tool results
)

// Closing-frame retries wait 0.5 s, doubling up to 4 s, like the desktop's
// closingRetry.
const (
	minClosingRetry = 500 * time.Millisecond
	maxClosingRetry = 4 * time.Second
)

// ErrPartialFile replaces a download whose chunks were dropped after a failed
// submit round, in the desktop's words (HEr).
//
//nolint:revive,staticcheck // user-facing sentence
const ErrPartialFile = "Part of the file did not upload from the user's computer, so the copy stopped. Copy the file again."

// closingBudget returns how long f is retried after a failed submit round,
// or 0 if it is dropped at once.
func closingBudget(f *Frame) time.Duration {
	switch v := f.Frame.(type) {
	case *aiserverv1.GrokBotUserComputerResponseFrame_Client:
		var msg agentv1.ExecClientMessage
		if (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(v.Client.GetMessageJson()), &msg) != nil {
			return 0
		}
		if s := msg.GetShellStream(); s != nil {
			switch s.Event.(type) {
			case *agentv1.ShellStream_Exit, *agentv1.ShellStream_Rejected, *agentv1.ShellStream_PermissionDenied,
				*agentv1.ShellStream_SandboxUnsupported, *agentv1.ShellStream_Backgrounded:
				return execBudget
			}
			return 0
		}
		if msg.Message != nil {
			return execBudget
		}
	case *aiserverv1.GrokBotUserComputerResponseFrame_Control:
		var msg agentv1.ExecClientControlMessage
		if (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal([]byte(v.Control.GetMessageJson()), &msg) == nil &&
			(msg.GetThrow() != nil || msg.GetStreamClose() != nil) {
			return execBudget
		}
	case *aiserverv1.GrokBotUserComputerResponseFrame_File:
		if v.File.GetLast() && v.File.GetSeq() == 0 {
			return fileBudget
		}
	case *aiserverv1.GrokBotUserComputerResponseFrame_FileError, *aiserverv1.GrokBotUserComputerResponseFrame_UrlTransfer:
		return fileBudget
	case *aiserverv1.GrokBotUserComputerResponseFrame_MessagesResult, *aiserverv1.GrokBotUserComputerResponseFrame_MessagesError,
		*aiserverv1.GrokBotUserComputerResponseFrame_MessagesConsentResult, *aiserverv1.GrokBotUserComputerResponseFrame_LocalToolResult:
		return messagesBudget
	}
	return 0
}

// Failure says what a failed submit round did with its batch.
type Failure struct {
	Kept    int // closing frames kept for another try
	Dropped int // frames dropped, including Expired
	Expired int // closing frames past their budget
}

// fail applies a failed round to batch, like the desktop: frames of canceled
// requests and frames that do not close their request are dropped; closing
// frames are kept until their budget, counted from the start of the round in
// which they first failed, runs out. A download that lost a chunk is replaced
// by a file error. If only kept closing frames remain, the next submit waits
// a growing delay, or until Add queues a frame other than a keep-alive.
func (q *Queue) fail(now time.Time, batch []*Frame, canceled func(string) bool) Failure {
	var out Failure
	broken := map[string]bool{}
	drop := map[*Frame]bool{}
	for _, f := range batch {
		budget := closingBudget(f)
		since, seen := q.closingSince[f]
		if !seen {
			since = q.roundStart
		}
		switch {
		case canceled(f.RequestId):
		case budget == 0:
			if f.GetFile() != nil {
				broken[f.RequestId] = true
			}
		case now.Sub(since) >= budget:
			out.Expired++
		default:
			q.closingSince[f] = since
			out.Kept++
			continue
		}
		drop[f] = true
		delete(q.closingSince, f)
		out.Dropped++
	}
	for _, f := range q.pending {
		if broken[f.RequestId] && !drop[f] {
			drop[f] = true
			delete(q.closingSince, f)
		}
	}
	q.remove(drop)
	for id := range broken {
		q.partial[id] = true
		q.Add(&Frame{RequestId: id, Frame: &aiserverv1.GrokBotUserComputerResponseFrame_FileError{
			FileError: &aiserverv1.GrokBotUserComputerFileError{Error: ErrPartialFile},
		}})
	}
	if out.Kept > 0 && len(q.pending) == out.Kept {
		q.closingRetries++
		q.closingWait = true
		q.nextSubmit = now.Add(min(maxClosingRetry, minClosingRetry<<min(q.closingRetries-1, 3)))
	}
	return out
}
