// Package scenario describes what a comparison test does to a client: the
// files it starts with, the requests it gets and what is recorded of its
// answers. A scenario is written once and run against grokbot-tunnel and
// against the desktop client.
package scenario

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/tests/compare/transcript"
)

// Scenario is one comparison.
type Scenario struct {
	// ID names the scenario, "group/name".
	ID string
	// Ref is the ID of the matching test in the manual test plan, if any.
	Ref string
	// About says what behavior the scenario is for.
	About string

	// Permission is the computer's permission mode: "always" (the default),
	// "ask" or "never".
	Permission string
	// ServerAuthoritative makes the backend the one that decides on
	// permissions.
	ServerAuthoritative bool
	// Shell is the user's shell, /bin/bash by default.
	Shell string
	// Files are created in the root before the client starts.
	Files []File
	// Home are created in the home directory before the client starts.
	Home []File

	// RootLink makes the root a symbolic link to a directory next to it.
	RootLink bool
	// Patches are the changes to the desktop's bundle this scenario needs
	// to reach the code it compares; see js/preload.cjs.
	Patches []string

	Steps []Step

	// Watch are paths in the root whose state after the steps is compared.
	Watch []string
	// Hello adds the hello of the watch stream to the comparison.
	Hello bool
	// Calls adds the calls the client makes on the channel to the
	// comparison: watch, poll and withdraw, with what they carry.
	Calls bool
	// Headers adds the HTTP headers of the first call of each kind.
	Headers bool
	// KeepAlive adds the frames a client sends to show that a request is
	// alive; a run of them counts as one.
	KeepAlive bool
	// OrderedOutput compares the order of stdout and stderr chunks too.
	OrderedOutput bool
	// Rules rewrite values that vary from run to run in this scenario.
	Rules []transcript.Rule
	// Timeout is how long a step may wait for its answers, 20 s by default.
	Timeout time.Duration
}

// Step is one thing the test does once the client is connected.
type Step struct {
	// Send queues the request frames in one poll.
	Send []*aiserverv1.GrokBotUserComputerRequestFrame
	// Await are the IDs of the requests to wait for.
	Await []string
	// Files are created or changed in the root.
	Files []File
	// Sleep waits.
	Sleep time.Duration
	// Fail makes the backend fail calls.
	Fail *Fault
	// DropWatch makes the backend end the watch stream.
	DropWatch bool
	// Stop ends the client with SIGTERM and waits for its exit.
	Stop bool
}

// Fault makes the next Times calls of an RPC (fakesand.Watch, ...) fail with
// a Connect code.
type Fault struct {
	RPC   string
	Times int
	Code  connect.Code
}

// Fail makes the backend fail the next times calls of rpc with code.
func Fail(rpc string, times int, code connect.Code) Step {
	return Step{Fail: &Fault{RPC: rpc, Times: times, Code: code}}
}

// DropWatch makes the backend end the watch stream.
func DropWatch() Step { return Step{DropWatch: true} }

// Stop ends the client and waits for its exit.
func Stop() Step { return Step{Stop: true} }

// Do sends the requests and waits until each is answered.
func Do(frames ...*aiserverv1.GrokBotUserComputerRequestFrame) Step {
	ids := make([]string, len(frames))
	for i, f := range frames {
		ids[i] = f.GetRequestId()
	}
	return Step{Send: frames, Await: ids}
}

// Send sends the requests without waiting.
func Send(frames ...*aiserverv1.GrokBotUserComputerRequestFrame) Step { return Step{Send: frames} }

// Await waits until each of the requests is answered.
func Await(ids ...string) Step { return Step{Await: ids} }

// Sleep waits for d.
func Sleep(d time.Duration) Step { return Step{Sleep: d} }

// Put creates or changes files in the root.
func Put(files ...File) Step { return Step{Files: files} }

// Digest identifies everything about the scenario that decides the answers.
func (s Scenario) Digest() string {
	type step struct {
		Send  []json.RawMessage
		Await []string
		Files []File
		Sleep time.Duration
		Fail  *Fault
		Drop  bool
		Stop  bool
	}
	steps := make([]step, len(s.Steps))
	for i, st := range s.Steps {
		steps[i] = step{Await: st.Await, Files: st.Files, Sleep: st.Sleep, Fail: st.Fail, Drop: st.DropWatch, Stop: st.Stop}
		for _, f := range st.Send {
			// The binary encoding of a frame is the same on every run;
			// protojson's is not.
			b, err := proto.MarshalOptions{Deterministic: true}.Marshal(f)
			if err != nil {
				panic(err)
			}
			q, err := json.Marshal(b)
			if err != nil {
				panic(err)
			}
			steps[i].Send = append(steps[i].Send, q)
		}
	}
	b, err := json.Marshal(struct {
		Permission          string
		ServerAuthoritative bool
		Shell               string
		Files, Home         []File
		Steps               []step
		Watch               []string
		Hello, Ordered      bool
		Patches             []string
		RootLink            bool
		Calls, Headers      bool
		KeepAlive           bool
	}{
		s.Permission, s.ServerAuthoritative, s.Shell, s.Files, s.Home, steps, s.Watch, s.Hello, s.OrderedOutput, s.Patches,
		s.RootLink, s.Calls, s.Headers, s.KeepAlive,
	})
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Expand returns a copy of f with the names $ROOT, $HOME and $REAL in its
// paths and commands replaced by the directories of the run: scenarios are
// written before the directories are known.
func Expand(f *aiserverv1.GrokBotUserComputerRequestFrame, dirs map[string]string) *aiserverv1.GrokBotUserComputerRequestFrame {
	pairs := make([]string, 0, 2*len(dirs))
	for name, dir := range dirs {
		// Exec arguments are JSON; a directory of the test needs no escaping.
		pairs = append(pairs, name, dir)
	}
	r := strings.NewReplacer(pairs...)
	f = proto.CloneOf(f)
	switch x := f.GetFrame().(type) {
	case *aiserverv1.GrokBotUserComputerRequestFrame_Exec:
		x.Exec.ServerMessageJson = r.Replace(x.Exec.GetServerMessageJson())
	case *aiserverv1.GrokBotUserComputerRequestFrame_Upload:
		x.Upload.Path = r.Replace(x.Upload.GetPath())
	case *aiserverv1.GrokBotUserComputerRequestFrame_Download:
		x.Download.Path = r.Replace(x.Download.GetPath())
	}
	return f
}

// Answered reports whether frames hold the last frame of an answer: the end
// of an exec stream, the last chunk of a file, an error or a result.
func Answered(frames []*aiserverv1.GrokBotUserComputerResponseFrame) bool {
	for _, f := range frames {
		switch {
		case f.GetClient() != nil, f.GetMessagesAccepted() != nil, f.GetLocalToolAccepted() != nil,
			f.GetLocalToolPermissionPending() != nil:
		case f.GetControl() != nil:
			var control struct {
				StreamClose, Throw json.RawMessage
			}
			undecodable := json.Unmarshal([]byte(f.GetControl().GetMessageJson()), &control) != nil
			if undecodable || control.StreamClose != nil || control.Throw != nil {
				return true
			}
		case f.GetFile() != nil:
			if f.GetFile().GetLast() {
				return true
			}
		default:
			return true
		}
	}
	return false
}

// compact is protojson without its random whitespace.
func compact(m proto.Message) string {
	b, err := protojson.Marshal(m)
	if err != nil {
		panic(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		panic(err)
	}
	b, err = json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
