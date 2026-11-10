// Package compare runs scenarios against grokbot-tunnel and the desktop
// client and compares what they do. See README.md.
package compare

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/tests/compare/scenario"
	"github.com/exa-pub/grokbot-tunnel/tests/compare/subject"
	"github.com/exa-pub/grokbot-tunnel/tests/compare/transcript"
	"github.com/exa-pub/grokbot-tunnel/tests/e2e/fakesand"
)

const (
	// connectTimeout is how long a client may take to open its watch stream.
	connectTimeout = 20 * time.Second
	// stepTimeout is how long a step waits for its answers by default.
	stepTimeout = 20 * time.Second
	// settle is how long the test listens for frames after the last answer.
	settle = 150 * time.Millisecond
)

// Run runs the scenario against the client in work/<scenario> and returns
// the normalized transcript. Both clients of a scenario get the same paths,
// one after the other.
func Run(t testing.TB, work string, sc scenario.Scenario, client subject.Subject) *transcript.Transcript {
	t.Helper()
	dir := filepath.Join(work, strings.ReplaceAll(sc.ID, "/", "_"))
	home, root, target := filepath.Join(dir, "home"), filepath.Join(dir, "root"), filepath.Join(dir, "real")
	prepare(t, dir, home, root, target, sc)
	dirs := map[string]string{"$ROOT": root, "$HOME": home, "$REAL": target}

	cloud := fakesand.New(t)
	cloud.ServerAuthoritative = sc.ServerAuthoritative
	env := subject.Env{
		BackendURL: cloud.URL, Home: home, Root: root, Shell: orDefault(sc.Shell, "/bin/bash"),
		Permission: orDefault(sc.Permission, "always"), ServerAuthoritative: sc.ServerAuthoritative,
		Patches: sc.Patches, Run: client.Name() + ":" + sc.ID,
	}
	running := client.Start(t, env)
	defer running.Stop()
	watching := func([]*aiserverv1.GrokBotUserComputerResponseFrame) bool { return cloud.Hello() != nil }
	if _, ok := cloud.Await(connectTimeout, watching); !ok {
		t.Fatalf("%s did not open a watch stream within %v:\n%s", client.Name(), connectTimeout, running.Log())
	}

	out := &transcript.Transcript{
		Scenario: sc.ID, Digest: sc.Digest(), Subject: client.Name(), Env: environment(client), Requests: map[string][]any{},
	}
	collect(cloud, out, steps(t, cloud, running, dirs, sc, out))
	// A client that ends before the scenario stops it has crashed.
	if running.Exited() && !slices.ContainsFunc(sc.Steps, func(step scenario.Step) bool { return step.Stop }) {
		t.Errorf("%s exited during the scenario:\n%s", client.Name(), running.Log())
	}
	if sc.Hello {
		out.Hello = message(t, cloud.Hello())
	}
	if sc.Calls || sc.Headers {
		// The calls of the shutdown belong to the record.
		running.Stop()
		out.Calls, out.Headers = calls(t, cloud, sc)
	}
	if len(sc.Watch) > 0 {
		files, err := transcript.Snapshot(root, sc.Watch)
		if err != nil {
			t.Fatal(err)
		}
		out.Files = files
	}
	if keep := os.Getenv("COMPARE_KEEP"); keep != "" {
		// For looking at what a client left: a copy of its root.
		to := filepath.Join(keep, client.Name(), sc.ID)
		if err := os.MkdirAll(filepath.Dir(to), 0o755); err == nil { //nolint:gosec // a directory of the test
			os.CopyFS(to, os.DirFS(root)) //nolint:errcheck // a copy for a person to look at
		}
	}
	running.Stop()
	if paths := cloud.Unknown(); len(paths) > 0 {
		t.Logf("%s made requests the fake backend does not serve: %v", client.Name(), paths)
	}
	transcript.Normalize(out, transcript.Options{
		Paths: map[string]string{root: "$ROOT", home: "$HOME", target: "$REAL"}, OrderedOutput: sc.OrderedOutput, Rules: sc.Rules,
		KeepAlive: sc.KeepAlive,
	})
	if t.Failed() {
		t.Logf("%s log:\n%s", client.Name(), running.Log())
	}
	return out
}

// collect records the frames the client sent, after listening a moment for
// late ones, and marks the requests that were waited for in vain.
func collect(cloud *fakesand.Server, out *transcript.Transcript, unanswered []string) {
	frames, _ := cloud.Await(settle, func([]*aiserverv1.GrokBotUserComputerResponseFrame) bool { return false })
	for _, f := range frames {
		out.Requests[f.GetRequestId()] = append(out.Requests[f.GetRequestId()], transcript.Frame(f))
	}
	for _, id := range unanswered {
		out.Requests[id] = append(out.Requests[id], map[string]any{"unanswered": true})
	}
}

// steps runs the steps of the scenario and returns the IDs of the requests
// that were waited for in vain.
func steps(
	t testing.TB, cloud *fakesand.Server, running *subject.Running, dirs map[string]string, sc scenario.Scenario,
	out *transcript.Transcript,
) []string {
	t.Helper()
	root := dirs["$ROOT"]
	var unanswered []string
	timeout := sc.Timeout
	if timeout == 0 {
		timeout = stepTimeout
	}
	for _, step := range sc.Steps {
		if err := scenario.Create(root, step.Files); err != nil {
			t.Fatal(err)
		}
		// A request with an ID used before is answered by frames that come
		// after it is sent, not by those of the earlier request.
		before, _ := cloud.Await(0, func([]*aiserverv1.GrokBotUserComputerResponseFrame) bool { return true })
		resent := map[string]bool{}
		for _, f := range step.Send {
			switch _, ok := out.Requests[f.GetRequestId()]; {
			case f.GetCancel() != nil:
			case ok:
				resent[f.GetRequestId()] = true
			default:
				out.Requests[f.GetRequestId()] = nil
			}
		}
		answers := func(frames []*aiserverv1.GrokBotUserComputerResponseFrame) []string {
			return pending(frames, len(before), resent, step.Await)
		}
		if len(step.Send) > 0 {
			frames := make([]*aiserverv1.GrokBotUserComputerRequestFrame, len(step.Send))
			for i, f := range step.Send {
				frames[i] = scenario.Expand(f, dirs)
			}
			cloud.Enqueue(frames...)
		}
		_, ok := cloud.Await(timeout, func(frames []*aiserverv1.GrokBotUserComputerResponseFrame) bool {
			return len(answers(frames)) == 0
		})
		if !ok {
			frames, _ := cloud.Await(0, func([]*aiserverv1.GrokBotUserComputerResponseFrame) bool { return true })
			unanswered = append(unanswered, answers(frames)...)
		}
		time.Sleep(step.Sleep)
		if step.Fail != nil {
			cloud.Fail(step.Fail.RPC, step.Fail.Times, step.Fail.Code)
		}
		if step.DropWatch {
			cloud.DropWatch()
		}
		if step.Stop {
			running.Stop()
		}
	}
	return unanswered
}

// pending returns the IDs that frames do not answer yet. For the IDs in
// resent only the frames from index from on count.
func pending(frames []*aiserverv1.GrokBotUserComputerResponseFrame, from int, resent map[string]bool, ids []string) []string {
	var out []string
	for _, id := range ids {
		var of []*aiserverv1.GrokBotUserComputerResponseFrame
		for i, f := range frames {
			if f.GetRequestId() == id && (i >= from || !resent[id]) {
				of = append(of, f)
			}
		}
		if !scenario.Answered(of) {
			out = append(out, id)
		}
	}
	return out
}

// prepare makes an empty dir with the home and root of the scenario.
func prepare(t testing.TB, dir, home, root, target string, sc scenario.Scenario) {
	t.Helper()
	// Fixtures may be unreadable or read-only.
	//nolint:errcheck // RemoveAll reports what could not be made removable
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(path, 0o700) //nolint:errcheck,gosec // a directory of the test
		}
		return nil
	})
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	dirs := []string{home, root}
	if sc.RootLink {
		dirs = []string{home, target}
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil { //nolint:gosec // a directory of the test
			t.Fatal(err)
		}
	}
	if sc.RootLink {
		if err := os.Symlink(target, root); err != nil {
			t.Fatal(err)
		}
	}
	if err := scenario.Create(root, sc.Files); err != nil {
		t.Fatal(err)
	}
	if err := scenario.Create(home, sc.Home); err != nil {
		t.Fatal(err)
	}
}

// calls returns the channel calls the client made, without the submits,
// whose frames are compared as the answers, and the headers of the first
// call of each kind.
func calls(t testing.TB, cloud *fakesand.Server, sc scenario.Scenario) ([]any, map[string]any) {
	t.Helper()
	var list []any
	headers := map[string]any{}
	polled := false // the call before was a poll
	for _, c := range cloud.Calls() {
		if _, seen := headers[c.RPC]; !seen && sc.Headers {
			headers[c.RPC] = header(c.Header)
		}
		if c.RPC == fakesand.Submit || !sc.Calls {
			continue
		}
		// A poll that acknowledges nothing right after another poll is one
		// a client makes to see whether more is queued, or on a timer: how
		// many of them there are depends on timing.
		poll, isPoll := c.Request.(*aiserverv1.PollGrokBotUserComputerRequestsRequest)
		idle := isPoll && polled && len(poll.GetAckIds()) == 0
		polled = isPoll
		if idle {
			continue
		}
		request, ok := message(t, c.Request).(map[string]any)
		if !ok {
			t.Fatalf("%s request is not an object", c.RPC)
		}
		// The hello is compared on its own; the machine ID and the presence
		// token are made up by each client.
		delete(request, "hello")
		for key, name := range map[string]string{"machineId": "<MACHINE>", "presenceToken": "<TOKEN>"} {
			if _, ok := request[key]; ok {
				request[key] = name
			}
		}
		list = append(list, map[string]any{"rpc": c.RPC, "request": request})
	}
	if !sc.Headers {
		headers = nil
	}
	return list, headers
}

// header returns the headers of a call that a client chooses. Those of the
// connection are left out: the test serves plain HTTP/1.1, which is not how
// either client reaches the real backend.
func header(h http.Header) map[string]any {
	out := map[string]any{}
	for name, values := range h {
		switch name = strings.ToLower(name); name {
		case "host", "connection", "content-length", "transfer-encoding":
		case "connect-timeout-ms":
			// What is left of a deadline.
			out[name] = "<MS>"
		default:
			out[name] = strings.Join(values, ", ")
		}
	}
	return out
}

func message(t testing.TB, hello proto.Message) any {
	t.Helper()
	b, err := protojson.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

var programs = sync.OnceValue(func() map[string]string {
	return map[string]string{
		"base": os.Getenv("GROKBOT_COMPARE_BASE"),
		"bash": subject.Version("bash", "-c", "echo $BASH_VERSION"),
		"zsh":  subject.Version("zsh", "-c", "echo $ZSH_VERSION"),
		"rg":   subject.Version("rg", "--version"),
		"git":  subject.Version("git", "--version"),
	}
})

// environment names the programs a client ran with: those of the image, and
// for the desktop its runtime.
func environment(client subject.Subject) map[string]string {
	out := map[string]string{}
	for k, v := range programs() {
		out[k] = v
	}
	if _, ok := client.(subject.Desktop); ok {
		out["runtime"] = "node " + subject.Version("node", "--version")
	}
	return out
}
