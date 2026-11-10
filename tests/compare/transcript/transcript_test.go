package transcript

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

// j parses JSON the way transcripts hold it.
func j(t *testing.T, s string) any {
	t.Helper()
	v := parse([]byte(s))
	require.NotNil(t, v, s)
	return v
}

func client(messageJSON string) *aiserverv1.GrokBotUserComputerResponseFrame {
	return &aiserverv1.GrokBotUserComputerResponseFrame{Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Client{
		Client: &aiserverv1.GrokBotUserComputerClientMessage{MessageJson: messageJSON},
	}}
}

// Two ways to write the same exec message give the same value: key order,
// defaults, proto field names, enums by number and numbers as strings.
func TestExecMessageIsCanonical(t *testing.T) {
	a := execMessage(`{"id":1,"shellStream":{"exit":{"code":0,"cwd":"/r","aborted":false,"abortReason":"SHELL_ABORT_REASON_TIMEOUT"}}}`,
		&agentv1.ExecClientMessage{})
	b := execMessage(`{"shell_stream":{"exit":{"abort_reason":2,"cwd":"/r"}},"id":"1","execId":""}`, &agentv1.ExecClientMessage{})
	assert.Equal(t, a, b)
	assert.Equal(t, j(t, `{"id":1,"shellStream":{"exit":{"cwd":"/r","abortReason":"SHELL_ABORT_REASON_TIMEOUT"}}}`), a)
}

// What presence tells apart stays: an optional field at its default and an
// empty message are not the same as nothing.
func TestExecMessageKeepsPresence(t *testing.T) {
	got := execMessage(`{"id":1,"localExecutionTimeMs":0,"shellStream":{"start":{}}}`, &agentv1.ExecClientMessage{})
	assert.Equal(t, j(t, `{"id":1,"localExecutionTimeMs":0,"shellStream":{"start":{}}}`), got)
}

// A field our schema does not have is kept, so that it shows as a
// difference; text that is not JSON is kept as it is.
func TestExecMessageKeepsWhatItDoesNotKnow(t *testing.T) {
	got := execMessage(`{"id":1,"futureField":{"x":0}}`, &agentv1.ExecClientMessage{})
	assert.Equal(t, j(t, `{"id":1,"futureField":{"x":0}}`), got)
	assert.Equal(t, map[string]any{"invalidJson": "{"}, execMessage("{", &agentv1.ExecClientMessage{}))
}

func TestFrame(t *testing.T) {
	state := "exists"
	control := &aiserverv1.GrokBotUserComputerResponseFrame{Frame: &aiserverv1.GrokBotUserComputerResponseFrame_Control{
		Control: &aiserverv1.GrokBotUserComputerControlMessage{MessageJson: `{"throw":{"id":0,"error":"no"}}`, CwdState: &state},
	}}
	assert.Equal(t, j(t, `{"control":{"throw":{"error":"no"}},"cwdState":"exists"}`), generic(Frame(control)))

	file := &aiserverv1.GrokBotUserComputerResponseFrame{RequestId: "r", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_File{
		File: &aiserverv1.GrokBotUserComputerFile{Data: []byte("hi"), Seq: 2, Last: true},
	}}
	assert.Equal(t, j(t, `{"file":{"seq":2,"last":true,"size":2,"text":"hi",
		"sha256":"8f434346648f6b96df89dda901c5176b10a6d83961dd3c1ac88b59b2dc327aa4"}}`), generic(Frame(file)))

	refused := &aiserverv1.GrokBotUserComputerResponseFrame{RequestId: "r", Frame: &aiserverv1.GrokBotUserComputerResponseFrame_FileError{
		FileError: &aiserverv1.GrokBotUserComputerFileError{Error: "no"},
	}}
	assert.Equal(t, j(t, `{"fileError":{"error":"no"}}`), generic(Frame(refused)))
}

func stdout(data string) any {
	return generic(Frame(client(`{"id":1,"shellStream":{"stdout":{"data":` + string(mustJSON(data)) + `}}}`)))
}

func stderr(data string) any {
	return generic(Frame(client(`{"id":1,"shellStream":{"stderr":{"data":` + string(mustJSON(data)) + `}}}`)))
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// How output is split into chunks is not compared; whether stdout came
// before stderr is, when the scenario asks.
func TestJoinOutput(t *testing.T) {
	start := generic(Frame(client(`{"id":1,"shellStream":{"start":{}}}`)))
	exit := generic(Frame(client(`{"id":1,"shellStream":{"exit":{}}}`)))
	frames := []any{start, stdout("a"), stderr("x"), stdout("b"), stdout("c"), exit}
	assert.Equal(t, []any{start, stdout("abc"), stderr("x"), exit}, joinOutput(frames, false))
	assert.Equal(t, []any{start, stdout("a"), stderr("x"), stdout("bc"), exit}, joinOutput(frames, true))
	assert.Equal(t, []any{start, exit}, joinOutput([]any{start, exit}, false))
}

func TestNormalize(t *testing.T) {
	tr := &Transcript{
		Requests: map[string][]any{"r1": {
			generic(Frame(client(`{"id":1,"localExecutionTimeMs":12,"shellStream":{"backgrounded":{"shellId":4711,"pid":99,"command":"x"}}}`))),
			map[string]any{"messagesAccepted": map[string]any{}},
			map[string]any{"control": map[string]any{"heartbeat": map[string]any{}}},
			map[string]any{"control": map[string]any{"throw": map[string]any{
				"error": "bad /work/root/a.txt", "stackTrace": "Error: bad\n    at f (main.cjs:1:2)\n    at g (main.cjs:3:4)",
			}}},
			stdout("see /work/root/terminals/4711.txt and /work/rootless and /work/home\n"),
		}},
		Files: map[string]any{
			"terminals/4711.txt": Blob([]byte("---\npid: 99\ncwd: \"/work/root\"\nstarted_at: 2026-11-10T00:15:32.082Z\n" +
				"running_for_ms: 4019     \n---\nout\n\n---\nexit_code: 0\nelapsed_ms: 4019\nended_at: 2026-11-10T00:15:36.101Z\n---\n")),
			"agent-tools/edabb85c-4b44-4a2c-9b35-c4b2d2ec666a.txt": Blob([]byte("x")),
		},
	}
	Normalize(tr, Options{Paths: map[string]string{"/work/root": "$ROOT", "/work/home": "$HOME"}})
	assert.Equal(t, j(t, `[
		{"client":{"id":1,"localExecutionTimeMs":"<MS>","shellStream":{"backgrounded":{"shellId":"<SHELL#1>","pid":"<PID>","command":"x"}}}},
		{"control":{"throw":{"error":"bad $ROOT/a.txt","stackTrace":"Error: bad"}}},
		{"client":{"id":1,"shellStream":{"stdout":{"data":"see $ROOT/terminals/<SHELL#1>.txt and /work/rootless and $HOME\n"}}}}
	]`), generic(tr.Requests["r1"]))
	assert.Equal(t, j(t, `{
		"terminals/<SHELL#1>.txt": {"text": "---\npid: <PID>\ncwd: \"$ROOT\"\nstarted_at: <TS>\nrunning_for_ms: <MS>     \n---\nout\n\n---\nexit_code: 0\nelapsed_ms: <MS>\nended_at: <TS>\n---\n"},
		"agent-tools/<UUID#1>.txt": {"size":1,"text":"x","sha256":"2d711642b726b04401627ca9fbac32f5c8530fb1903cc4db02258717921a4881"}
	}`), generic(tr.Files))
}

// Keep-alive frames are dropped, or kept as one of each kind a run: a
// heartbeat between two messages_accepted must not hide behind them.
func TestKeepAlive(t *testing.T) {
	accepted := map[string]any{"messagesAccepted": map[string]any{}}
	heartbeat := map[string]any{"control": map[string]any{"heartbeat": map[string]any{"id": json.Number("1")}}}
	start, exit := stdout("start"), stdout("exit")
	frames := []any{accepted, accepted, start, heartbeat, accepted, heartbeat, exit, heartbeat}
	assert.Equal(t, []any{start, exit}, keepAlive(frames, false))
	assert.Equal(t, []any{accepted, start, accepted, heartbeat, exit, heartbeat}, keepAlive(frames, true))
}

// A scenario's own rules: a pattern, a whole value and a function.
func TestNormalizeRules(t *testing.T) {
	tr := &Transcript{Requests: map[string][]any{"r1": {stdout("A=1\nB=2\n"), map[string]any{"list": []any{"a", "b"}, "n": json.Number("5")}}}}
	Normalize(tr, Options{Rules: []Rule{
		{Path: "requests/r1/*/client/shellStream/stdout/data", Pattern: regexp.MustCompile(`(?m)^A=.*\n`)},
		{Path: "requests/**/n", Replace: "<N>"},
		{Path: "requests/**/list", Func: func(v any) any { return len(v.([]any)) }},
	}})
	assert.Equal(t, j(t, `[{"client":{"id":1,"shellStream":{"stdout":{"data":"B=2\n"}}}}, {"list":2,"n":"<N>"}]`), generic(tr.Requests["r1"]))
}

// The running time keeps the width of its field.
func TestTerminalTextKeepsWidth(t *testing.T) {
	assert.Equal(t, "---\npid: <PID>\nrunning_for_ms: <MS>  \n", terminalText("---\npid: 5\nrunning_for_ms: 12    \n"))
	assert.Equal(t, "running_for_ms: <MS>\n", terminalText("running_for_ms: 12\n"))
}

// Megabytes of output are described, not kept.
func TestShorten(t *testing.T) {
	got := shorten(strings.Repeat("a", longText) + "end").(map[string]any)
	assert.Equal(t, json.Number("8195"), got["length"])
	assert.Equal(t, strings.Repeat("a", textEdge), got["start"])
	assert.True(t, strings.HasSuffix(got["end"].(string), "aend"))
	assert.Equal(t, "short", shorten("short"))
}

func TestMatchPath(t *testing.T) {
	for _, tc := range []struct {
		pattern, path string
		want          bool
	}{
		{"a/b", "a/b", true}, {"a/*", "a/b", true}, {"a/*", "a/b/c", false}, {"a/**", "a", true}, {"a/**", "a/b/c", true},
		{"a/**/z", "a/z", true}, {"a/**/z", "a/b/c/z", true}, {"a/**/z", "a/b/c", false}, {"*/c", "a/b/c", false},
	} {
		assert.Equal(t, tc.want, matchPath(strings.Split(tc.pattern, "/"), strings.Split(tc.path, "/")), tc.pattern+" "+tc.path)
	}
}

func paths(diffs []Difference) []string {
	out := make([]string, len(diffs))
	for i, d := range diffs {
		out[i] = d.Path
	}
	return out
}

func TestDiff(t *testing.T) {
	a := j(t, `{"requests":{"r1":[{"a":1},{"b":{"x":1,"y":2}},{"c":3},{"d":4}]},"files":null}`)
	b := j(t, `{"requests":{"r1":[{"a":1},{"b":{"x":1,"y":9,"z":0}},{"d":4}]},"files":{"f":1}}`)
	diffs := Diff(a, b)
	// One frame less shifts nothing: the frames the lists share at their
	// ends are matched first.
	assert.Equal(t, []string{"files", "requests/r1/1/b/y", "requests/r1/1/b/z", "requests/r1/2"}, paths(diffs))
	assert.Nil(t, diffs[0].Desktop)
	assert.JSONEq(t, `{"f":1}`, string(*diffs[0].Go))
	assert.JSONEq(t, `2`, string(*diffs[1].Desktop))
	assert.JSONEq(t, `9`, string(*diffs[1].Go))
	assert.JSONEq(t, `{"c":3}`, string(*diffs[3].Desktop))
	assert.Nil(t, diffs[3].Go)
	assert.Empty(t, Diff(a, a))
}

func TestKnownDifferences(t *testing.T) {
	file := filepath.Join(t.TempDir(), "known-diffs.json")
	require.NoError(t, os.WriteFile(file, []byte(`[
		{"scenario": "shell/a", "path": "requests/r1/*/x", "reason": "by design"},
		{"scenario": "shell/*", "path": "files/**", "go": "0600", "reason": "private files"},
		{"scenario": "read/gone", "path": "requests/**", "reason": "fixed since"}
	]`), 0o600))
	r, err := LoadKnown(file)
	require.NoError(t, err)

	x, mode, other := json.RawMessage(`1`), json.RawMessage(`"0600"`), json.RawMessage(`"0644"`)
	known, unexpected := r.Split("shell/a", []Difference{
		{Path: "requests/r1/0/x", Go: &x}, {Path: "requests/r2/0/x", Go: &x},
		{Path: "files/t/mode", Go: &mode}, {Path: "files/u/mode", Go: &other},
	})
	assert.Equal(t, []string{"requests/r1/0/x", "files/t/mode"}, paths(known))
	assert.Equal(t, []string{"requests/r2/0/x", "files/u/mode"}, paths(unexpected))

	// An entry is stale once every scenario it names ran without it; while
	// some did not run, it may still be needed.
	all := []string{"shell/a", "shell/b", "read/gone"}
	assert.Empty(t, r.Stale(all))
	r.Split("read/gone", nil)
	stale := r.Stale(all)
	require.Len(t, stale, 1)
	assert.Equal(t, "read/gone", stale[0].Scenario)
	// An entry for a scenario that does not exist is stale too.
	assert.Len(t, r.Stale([]string{"shell/a", "shell/b"}), 1)
}

func TestLoadKnownRefusesIncompleteEntries(t *testing.T) {
	file := filepath.Join(t.TempDir(), "known-diffs.json")
	require.NoError(t, os.WriteFile(file, []byte(`[{"scenario": "a/b", "path": "x"}]`), 0o600))
	_, err := LoadKnown(file)
	require.ErrorContains(t, err, "needs scenario, path and reason")
	r, err := LoadKnown(filepath.Join(t.TempDir(), "missing.json"))
	require.NoError(t, err)
	assert.Empty(t, r.Stale(nil))
}

func TestSnapshot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "d", "sub"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(root, "d", "a.txt"), []byte("a"), 0o640))
	require.NoError(t, os.Symlink("a.txt", filepath.Join(root, "d", "link")))
	got, err := Snapshot(root, []string{"d", "none"})
	require.NoError(t, err)
	assert.Equal(t, j(t, `{
		"d": {"dir": true, "mode": "0750"}, "d/sub": {"dir": true, "mode": "0750"}, "d/link": {"link": "a.txt"},
		"d/a.txt": {"mode": "0640", "size": 1, "text": "a", "sha256": "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb"},
		"none": {"missing": true}
	}`), generic(got))
}
