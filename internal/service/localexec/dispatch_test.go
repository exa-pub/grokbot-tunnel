package localexec

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
)

func clientMessages(t *testing.T, frames []*aiserverv1.GrokBotUserComputerResponseFrame) []*agentv1.ExecClientMessage {
	t.Helper()
	out := []*agentv1.ExecClientMessage{}
	for _, f := range frames {
		if f.GetClient() != nil {
			m := &agentv1.ExecClientMessage{}
			if err := protojson.Unmarshal([]byte(f.GetClient().MessageJson), m); err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
	}
	return out
}
func controlMessage(t *testing.T, f *aiserverv1.GrokBotUserComputerResponseFrame) *agentv1.ExecClientControlMessage {
	t.Helper()
	if f.GetControl() == nil {
		t.Fatalf("expected control: %v", f)
	}
	m := &agentv1.ExecClientControlMessage{}
	if err := protojson.Unmarshal([]byte(f.GetControl().MessageJson), m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLifecycleBranches(t *testing.T) {
	e := setup(t)
	os.WriteFile(filepath.Join(e.Root, "data"), []byte("ok"), 0600)
	first := collect(t, e, execFrame(`{"id":999,"execId":"external","readArgs":{"path":"data"}}`))
	messages := clientMessages(t, first)
	if len(messages) != 1 || messages[0].Id != 1 || messages[0].ExecId != "external" || messages[0].LocalExecutionTimeMs == nil || controlMessage(t, first[len(first)-1]).GetStreamClose().Id != 1 {
		t.Fatal(first)
	}
	missing := collect(t, e, execFrame(`{"id":999,"readArgs":{"path":"missing"}}`))
	if clientMessages(t, missing)[0].GetReadResult().GetFileNotFound() == nil || controlMessage(t, missing[len(missing)-1]).GetStreamClose().Id != 2 {
		t.Fatal(missing)
	}
	unsupported := collect(t, e, execFrame(`{"id":999,"writeArgs":{"path":"x"}}`))
	if len(unsupported) != 2 || controlMessage(t, unsupported[0]).GetThrow().Id != 3 || controlMessage(t, unsupported[1]).GetStreamClose().Id != 3 {
		t.Fatal(unsupported)
	}
	e.gate = gate("never")
	denied := collect(t, e, execFrame(`{"id":999,"readArgs":{"path":"data"}}`))
	if len(denied) != 1 || controlMessage(t, denied[0]).GetThrow().Id != 0 {
		t.Fatal(denied)
	}
	e.gate = gate("always")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var canceled []*aiserverv1.GrokBotUserComputerResponseFrame
	_ = e.Handle(ctx, execFrame(`{"readArgs":{"path":"data"}}`), false, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error { canceled = append(canceled, f); return nil })
	if len(canceled) != 1 || controlMessage(t, canceled[0]).GetStreamClose() == nil {
		t.Fatal(canceled)
	}
	var calls int
	err := e.Handle(context.Background(), execFrame(`{"readArgs":{"path":"data"}}`), false, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error { calls++; return errors.New("sink failed") })
	if err == nil || calls != 1 {
		t.Fatalf("sink failure retried: %v %d", err, calls)
	}
}
func TestShellStateCwdAndUTF8(t *testing.T) {
	e := setup(t)
	os.Mkdir(filepath.Join(e.Root, "child"), 0700)
	first := collect(t, e, execFrame(`{"shellStreamArgs":{"command":"export GROK_TEST_PERSIST=hello; cd child"}}`))
	msgs := clientMessages(t, first)
	exit := msgs[len(msgs)-1].GetShellStream().GetExit()
	if exit.Cwd != filepath.Join(e.Root, "child") {
		t.Fatal(exit)
	}
	second := collect(t, e, execFrame(`{"shellStreamArgs":{"command":"printf '%s|' \"$GROK_TEST_PERSIST\"; pwd"}}`))
	var stdout strings.Builder
	for _, m := range clientMessages(t, second) {
		stdout.WriteString(m.GetShellStream().GetStdout().GetData())
	}
	if stdout.String() != "hello|"+filepath.Join(e.Root, "child")+"\n" {
		t.Fatal(stdout.String())
	}
}
func TestExecHeartbeatAndTerminalOrder(t *testing.T) {
	e := setup(t)
	var frames []*aiserverv1.GrokBotUserComputerResponseFrame
	var mu sync.Mutex
	err := e.Handle(context.Background(), execFrame(`{"shellStreamArgs":{"command":"sleep 3.1"}}`), false, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
		mu.Lock()
		defer mu.Unlock()
		frames = append(frames, f)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	beats := 0
	for _, f := range frames {
		if f.GetControl() != nil && controlMessage(t, f).GetHeartbeat() != nil {
			beats++
		}
	}
	if beats < 1 || controlMessage(t, frames[len(frames)-1]).GetStreamClose() == nil {
		t.Fatal("missing heartbeat/close", frames)
	}
}

func TestReadTypedErrorsEncodingAndEscapedLimit(t *testing.T) {
	e := setup(t)
	os.Mkdir(filepath.Join(e.Root, "dir"), 0700)
	frames := collect(t, e, execFrame(`{"readArgs":{"path":"dir"}}`))
	if clientMessages(t, frames)[0].GetReadResult().GetInvalidFile() == nil {
		t.Fatal("directory read not typed")
	}
	os.WriteFile(filepath.Join(e.Root, "utf16.txt"), []byte{0xff, 0xfe, 'h', 0, 'i', 0}, 0600)
	frames = collect(t, e, execFrame(`{"readArgs":{"path":"utf16.txt"}}`))
	if clientMessages(t, frames)[0].GetReadResult().GetSuccess().GetContent() != "hi" {
		t.Fatal(frames)
	}
	os.WriteFile(filepath.Join(e.Root, "large.txt"), bytes.Repeat([]byte("\t"), execstream.MaxJSON), 0600)
	frames = collect(t, e, execFrame(`{"readArgs":{"path":"large.txt"}}`))
	m := clientMessages(t, frames)[0]
	if !m.GetReadResult().GetSuccess().Truncated || len(frames[0].GetClient().MessageJson) > execstream.MaxJSON {
		t.Fatal("JSON escaped limit not respected")
	}
}

// Like the desktop's, ls fills no TerminalMetadata for terminal files.
func TestListingTimeoutAndTerminalMetadata(t *testing.T) {
	e := setup(t)
	frames := collect(t, e, execFrame(`{"lsArgs":{"path":".","timeoutMs":0}}`))
	if clientMessages(t, frames)[0].GetLsResult().GetTimeout() == nil {
		t.Fatal("timeout did not return partial tree")
	}
	waitTerminal(t, e, collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"printf terminal-ok"}}`)))
	frames = collect(t, e, execFrame(`{"lsArgs":{"path":"terminals"}}`))
	tree := clientMessages(t, frames)[0].GetLsResult().GetSuccess().DirectoryTreeRoot
	if len(tree.ChildrenFiles) != 1 || tree.ChildrenFiles[0].TerminalMetadata != nil {
		t.Fatal(tree)
	}
}

func TestShellOptionsPersist(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	e := setup(t)
	collect(t, e, execFrame(`{"shellStreamArgs":{"command":"set -o noclobber; shopt -s nullglob"}}`))
	frames := collect(t, e, execFrame(`{"shellStreamArgs":{"command":"if [[ -o noclobber ]] && shopt -q nullglob; then printf options-ok; fi"}}`))
	var output strings.Builder
	for _, m := range clientMessages(t, frames) {
		output.WriteString(m.GetShellStream().GetStdout().GetData())
		if stderr := m.GetShellStream().GetStderr().GetData(); stderr != "" {
			t.Errorf("state replay stderr: %s", stderr)
		}
	}
	if output.String() != "options-ok" {
		t.Fatal(output.String(), e.session.State())
	}
}

func TestShellFunctionsPersistAliasesDoNot(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	e := setup(t)
	collect(t, e, execFrame(`{"shellStreamArgs":{"command":"shopt -s expand_aliases; alias grok_hi='printf alias-ok'; grok_fn() { printf function-ok; }"}}`))
	frames := collect(t, e, execFrame(`{"shellStreamArgs":{"command":"grok_hi 2>/dev/null || printf no-alias; printf '|'; grok_fn"}}`))
	var output strings.Builder
	for _, m := range clientMessages(t, frames) {
		output.WriteString(m.GetShellStream().GetStdout().GetData())
		if stderr := m.GetShellStream().GetStderr().GetData(); stderr != "" {
			t.Error(stderr)
		}
	}
	if output.String() != "no-alias|function-ok" {
		t.Fatal(output.String())
	}
}
