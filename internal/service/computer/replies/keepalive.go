package replies

import (
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
)

// KeepAlive tells the server that requests are still being handled: tasks
// maps the requests the caller holds to whether they started. A request gets
// at most one pending keep-alive, and none while it waits unstarted, once its
// stream is closed or after it was canceled. Requests that finished but whose
// frames are still pending are kept alive too.
//
// ≠ desktop: it keeps alive the backlog, unsent replies and the submit in
// flight, but not running requests (grok-bot-map.md, invariant 11).
func (q *Queue) KeepAlive(tasks map[string]bool, canceled func(id string) bool) {
	ids := map[string]bool{}
	for id, started := range tasks {
		ids[id] = started
	}
	for _, f := range q.pending {
		if _, ok := tasks[f.RequestId]; !ok {
			ids[f.RequestId] = true // finished, results not yet delivered
		}
	}
	for _, f := range q.pending {
		if f.GetMessagesAccepted() != nil || frames.Closes(f) {
			ids[f.RequestId] = false
		}
	}
	for id, alive := range ids {
		if !alive || canceled(id) {
			continue
		}
		q.Add(&Frame{
			RequestId: id,
			Frame: &aiserverv1.GrokBotUserComputerResponseFrame_MessagesAccepted{
				MessagesAccepted: &aiserverv1.GrokBotUserComputerMessagesAccepted{},
			},
		})
	}
}
