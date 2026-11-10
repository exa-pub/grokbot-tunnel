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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
	"github.com/exa-pub/grokbot-tunnel/internal/service/permission"
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
	// L6: like the desktop, the server's execId is not echoed.
	if len(messages) != 1 || messages[0].Id != 1 || messages[0].ExecId != "" || messages[0].LocalExecutionTimeMs == nil || controlMessage(t, first[len(first)-1]).GetStreamClose().Id != 1 {
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

func TestShellFunctionsAndAliasesPersist(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	e := setup(t)
	collect(t, e, execFrame(`{"shellStreamArgs":{"command":"alias grok_hi='printf alias-ok'; grok_fn() { printf function-ok; }"}}`))
	frames := collect(t, e, execFrame(`{"shellStreamArgs":{"command":"grok_hi 2>/dev/null || printf no-alias; printf '|'; grok_fn"}}`))
	var output strings.Builder
	for _, m := range clientMessages(t, frames) {
		output.WriteString(m.GetShellStream().GetStdout().GetData())
		if stderr := m.GetShellStream().GetStderr().GetData(); stderr != "" {
			t.Error(stderr)
		}
	}
	if output.String() != "alias-ok|function-ok" {
		t.Fatal(output.String())
	}
}

// L3: like the desktop, which checks the cancellation after the permission
// check, a check that ended with the cancellation closes the request with
// streamClose{0} instead of throwing; a refusal still throws.
func TestGateCanceledClosesRequest(t *testing.T) {
	e := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	e.gate = func(ctx context.Context, _ string, _, _ bool) error {
		cancel()
		return ctx.Err()
	}
	var out []*aiserverv1.GrokBotUserComputerResponseFrame
	require.NoError(t, e.Handle(ctx, execFrame(`{"readArgs":{"path":"x"}}`), false, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
		out = append(out, f)
		return nil
	}))
	require.Len(t, out, 1)
	closed := controlMessage(t, out[0]).GetStreamClose()
	require.NotNil(t, closed)
	assert.Zero(t, closed.GetId())

	e.gate = gate(entity.ModeNever)
	canceled, stop := context.WithCancel(context.Background())
	stop()
	out = nil
	require.NoError(t, e.Handle(canceled, execFrame(`{"readArgs":{"path":"x"}}`), false, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
		out = append(out, f)
		return nil
	}))
	require.Len(t, out, 1)
	assert.Equal(t, permission.ErrDisabled.Error(), controlMessage(t, out[0]).GetThrow().GetError())
}

// L5: the case of an unsupported request is the first key of the server
// message that is a case of the desktop's message, by its JSON or protobuf
// name.
func TestExecCase(t *testing.T) {
	for in, want := range map[string]string{
		`{"id":1,"execId":"x","spanContext":{},"machineId":"m","acceptHookAdditionalContexts":true,"writeArgs":{}}`: "writeArgs",
		`{"grep_args":{"pattern":"x"}}`:      "grepArgs",
		`{"gitDiffRequest":{}}`:              "gitDiffRequest",
		`{"unknownArgs":{},"fetchArgs":{}}`:  "fetchArgs",
		`{"id":1}`:                           "",
		`{"somethingNew":{"readArgs":{}}}`:   "",
		`[]`:                                 "",
		`{"readArgs":{"path":"x"},"bad":`:    "readArgs",
		`{"mini_swe_agent_bash_args":null}`:  "miniSweAgentBashArgs",
		`{"redactedReadArgs":{"path":"x"}}`:  "redactedReadArgs",
		`{"computerUseArgs":{"action":"x"}}`: "computerUseArgs",
	} {
		assert.Equal(t, want, execCase(in), in)
	}
}

// L5: the throw of an unsupported request carries the desktop's text for its
// case, with cwd_state "unset" (L4), and no stack trace.
func TestUnsupportedTexts(t *testing.T) {
	e := setup(t)
	for in, want := range map[string]string{
		`{"id":7,"execId":"e","grepArgs":{"pattern":"x"}}`: "No handler found for server message of type grepArgs",
		`{"somethingNew":{}}`:                              "No handler found for server message of type undefined",
		`{"recordScreenArgs":{}}`: "Screen recording is not enabled for this environment, so recordings cannot be captured here. " +
			"Do not retry; skip the recording and present other evidence instead. " +
			"(No record-screen handler is registered on this exec daemon.)",
	} {
		out := collect(t, e, execFrame(in))
		require.Len(t, out, 2, in)
		throw := controlMessage(t, out[0]).GetThrow()
		require.NotNil(t, throw, in)
		assert.Equal(t, want, throw.GetError(), in)
		assert.Nil(t, throw.StackTrace, in)
		assert.Equal(t, "unset", out[0].GetControl().GetCwdState(), in)
		assert.NotNil(t, controlMessage(t, out[1]).GetStreamClose(), in)
	}
}

// askingPrompter fails the test if it is asked anything.
type askingPrompter struct{ t *testing.T }

func (askingPrompter) Interactive() bool { return true }

func (p askingPrompter) Ask(_ context.Context, question string) (string, error) {
	p.t.Errorf("asked %q", question)
	return "y", nil
}

// L13: like the desktop, which cannot describe an unsupported request, the
// user is not asked about it: it is refused as not approved.
func TestUnsupportedNotPrompted(t *testing.T) {
	e := setup(t)
	e.gate = (&permission.Policy{Mode: entity.ModeAsk, Prompter: askingPrompter{t}}).Allow
	out := collect(t, e, execFrame(`{"writeArgs":{"path":"x"}}`))
	require.Len(t, out, 1)
	throw := controlMessage(t, out[0]).GetThrow()
	require.NotNil(t, throw)
	assert.Zero(t, throw.GetId())
	assert.Equal(t, permission.ErrRefused.Error(), throw.GetError())
}

// L13: the prompts of transfers and discovery quote the server's strings.
func TestParseQuotesTransferPaths(t *testing.T) {
	for f, want := range map[*aiserverv1.GrokBotUserComputerRequestFrame]string{
		{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Upload{Upload: &aiserverv1.GrokBotUserComputerUpload{Path: "a b\n"}}}:                           `Write "a b\n"`,
		{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Download{Download: &aiserverv1.GrokBotUserComputerDownload{Path: "a b"}}}:                       `Read "a b"`,
		{Frame: &aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool{DiscoverAiTool: &aiserverv1.GrokBotUserComputerDiscoverAiTool{Source: "codex"}}}: `Read AI tool setup and recent history for "codex" in the home/config directories (may be outside root)`,
	} {
		req, err := parse(f)
		require.NoError(t, err)
		assert.Equal(t, want, req.action)
	}
}

// L9: like the desktop's dispatcher (ilt), a working directory that cannot
// be checked fails the request with Node's text and no cwd_state, instead of
// running the command in Root; a missing one runs in Root with the warning.
func TestShellCwdResolvedBeforeRun(t *testing.T) {
	e := setup(t)
	out := collect(t, e, execFrame(`{"shellStreamArgs":{"command":"pwd","workingDirectory":" nope "}}`))
	msgs := clientMessages(t, out)
	require.GreaterOrEqual(t, len(msgs), 3)
	assert.Equal(t, "working directory  nope  does not exist on this machine; running in "+e.Root+" instead\n",
		msgs[0].GetShellStream().GetStderr().GetData())
	assert.NotNil(t, msgs[1].GetShellStream().GetStart())
	assert.Equal(t, e.Root+"\n", msgs[2].GetShellStream().GetStdout().GetData())

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	locked := filepath.Join(e.Root, "locked")
	require.NoError(t, os.Mkdir(locked, 0o700))
	require.NoError(t, os.Chmod(locked, 0))
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	for _, args := range []string{"shellStreamArgs", "backgroundShellSpawnArgs"} {
		out = collect(t, e, execFrame(`{"`+args+`":{"command":"pwd","workingDirectory":"locked/sub"}}`))
		require.Len(t, out, 1, args)
		throw := controlMessage(t, out[0]).GetThrow()
		require.NotNil(t, throw, args)
		want := "EACCES: permission denied, stat '" + filepath.Join(locked, "sub") + "'"
		assert.Equal(t, want, throw.GetError(), args)
		assert.Equal(t, "Error: "+want, throw.GetStackTrace(), args)
		assert.Nil(t, out[0].GetControl().CwdState, args)
	}
}
