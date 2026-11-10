package scenarios

import (
	"fmt"
	"time"

	"connectrpc.com/connect"

	. "github.com/exa-pub/grokbot-tunnel/tests/compare/scenario" //nolint:revive,staticcheck // scenarios read as a list of steps
	"github.com/exa-pub/grokbot-tunnel/tests/e2e/fakesand"
)

// hellos is a scenario for every permission mode and for who decides: what
// the client says about itself when it opens the watch stream.
func hellos() []Scenario {
	modes := []string{"always", "ask", "never"}
	out := make([]Scenario, 0, 2*len(modes))
	for _, mode := range modes {
		for _, authoritative := range []bool{false, true} {
			who := "local"
			if authoritative {
				who = "server"
			}
			out = append(out, Scenario{
				ID: fmt.Sprintf("channel/hello-%s-%s", mode, who), About: "the hello of the watch stream",
				Permission: mode, ServerAuthoritative: authoritative, Hello: true,
				Steps: []Step{Sleep(300 * time.Millisecond)},
			})
		}
	}
	return out
}

// sleeps is n shell requests that each sleep, with the IDs prefix1...
func sleeps(prefix string, n int, seconds string) []Frame {
	out := make([]Frame, n)
	for i := range out {
		out[i] = Shell(fmt.Sprintf("%s%02d", prefix, i+1), "sleep "+seconds+"; echo done")
	}
	return out
}

func ids(frames []Frame) []string {
	out := make([]string, len(frames))
	for i, f := range frames {
		out[i] = f.GetRequestId()
	}
	return out
}

var channel = append(hellos(), []Scenario{
	{
		ID: "channel/headers", About: "the HTTP headers of each call of the channel", Headers: true,
		Steps: []Step{Do(Shell("r1", "echo hello"))},
	},
	{
		ID: "channel/calls", About: "watch, poll with its acknowledgements and withdraw around one request", Calls: true,
		Steps: []Step{
			Do(Shell("r1", "echo one")), Sleep(500 * time.Millisecond), Do(Shell("r2", "echo two")), Sleep(500 * time.Millisecond),
		},
	},
	{
		ID: "channel/batch", About: "three requests in one poll", Calls: true, Rules: anyExecID,
		Steps: []Step{Do(Shell("r1", "echo 1"), Read("r2", "missing"), Shell("r3", "echo 3")), Sleep(500 * time.Millisecond)},
	},
	func() Scenario {
		// More requests at once than the client runs at once.
		busy := sleeps("a", 12, "2")
		return Scenario{
			ID: "channel/in-flight", About: "twelve requests at once: eight run, the others wait", Rules: anyExecID,
			Steps: []Step{Send(busy...), Await(ids(busy)...)}, Timeout: 40 * time.Second,
		}
	}(),
	func() Scenario {
		// More requests at once than the client takes at all.
		busy := sleeps("a", 80, "3")
		return Scenario{
			ID: "channel/backlog", About: "eighty requests at once: those beyond the backlog are refused", Rules: anyExecID,
			Steps: []Step{Send(busy...), Await(ids(busy)...)}, Timeout: 90 * time.Second,
		}
	}(),
	{
		ID: "channel/keep-alive", About: "a request that runs for a while is kept alive", KeepAlive: true,
		Steps: []Step{Do(Shell("r1", "sleep 6; echo done"))},
	},
	func() Scenario {
		// Long enough for the ninth request to show that it is alive both
		// while it waits and once it runs.
		busy := sleeps("a", 9, "7")
		return Scenario{
			ID: "channel/keep-alive-waiting", About: "a request that waits for its turn is kept alive", KeepAlive: true,
			Steps: []Step{Send(busy...), Await(ids(busy)...)}, Timeout: 40 * time.Second, Rules: anyExecID,
		}
	}(),
	func() Scenario {
		busy := sleeps("a", 9, "3")
		return Scenario{
			ID: "channel/cancel-waiting", About: "a request that waits for its turn is canceled", Rules: anyExecID,
			Steps:   []Step{Send(busy...), Sleep(500 * time.Millisecond), Send(Cancel("a09")), Await(ids(busy)...)},
			Timeout: 40 * time.Second,
		}
	}(),
	{
		ID: "channel/cancel-first", About: "a cancel that arrives in the same poll as its request, before it",
		Steps: []Step{Send(Cancel("r1"), Shell("r1", "echo ran > ran.txt; echo hello")), Sleep(time.Second), Do(Shell("r2", "echo next"))},
		Watch: []string{"ran.txt"},
	},
	{
		ID: "channel/cancel-same-poll", About: "a cancel that arrives in the same poll as its request, after it",
		Steps: []Step{Send(Shell("r1", "sleep 5; echo late"), Cancel("r1")), Sleep(time.Second), Do(Shell("r2", "echo next"))},
	},
	{
		ID: "channel/cancel-finished", About: "a cancel of a request that has ended",
		Steps: []Step{Do(Shell("r1", "echo done")), Send(Cancel("r1")), Sleep(500 * time.Millisecond), Do(Shell("r2", "echo next"))},
	},
	{
		ID: "channel/cancel-redelivered", About: "a request is canceled and then arrives again",
		Steps: []Step{
			Do(Shell("r1", "echo done")), Send(Cancel("r1")), Sleep(1500 * time.Millisecond),
			Do(Shell("r1", "echo again")), Do(Shell("r2", "echo next")),
		},
	},
	{
		ID: "channel/stop-running", About: "the client is stopped while a command runs", Calls: true,
		Steps: []Step{Send(Shell("r1", "echo start; sleep 30; echo late")), Sleep(time.Second), Stop()},
	},
	{
		ID: "channel/stop-idle", About: "the client is stopped while it waits", Calls: true,
		Steps: []Step{Sleep(500 * time.Millisecond), Stop()},
	},
	{
		ID: "channel/watch-dropped", About: "the watch stream ends: the client dials again and serves on", Calls: true,
		Steps: []Step{
			Do(Shell("r1", "echo before")), DropWatch(), Sleep(3 * time.Second), Do(Shell("r2", "echo after")),
			Sleep(500 * time.Millisecond),
		},
		Timeout: 40 * time.Second,
	},
	{
		ID: "channel/submit-fails", About: "a submit that fails is sent again",
		Steps:   []Step{Fail(fakesand.Submit, 1, connect.CodeUnavailable), Do(Shell("r1", "echo hello")), Do(Shell("r2", "echo next"))},
		Timeout: 40 * time.Second,
	},
	{
		ID: "channel/poll-fails", About: "a poll that fails is made again",
		Steps:   []Step{Fail(fakesand.Poll, 1, connect.CodeUnavailable), Do(Shell("r1", "echo hello")), Do(Shell("r2", "echo next"))},
		Timeout: 40 * time.Second,
	},
}...)
