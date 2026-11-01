package localexec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/exa-pub/grokbot-tunnel/internal/entity"
	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
	"github.com/exa-pub/grokbot-tunnel/internal/service/permission"
)

// gate returns the Gate of a policy with mode that never asks.
func gate(mode entity.Mode) Gate { return (&permission.Policy{Mode: mode}).Allow }

func setup(t *testing.T) *Executor {
	t.Helper()
	return setupHome(t, t.TempDir())
}

// setupHome isolates shells from the user's profile: HOME and ZDOTDIR point to
// home.
func setupHome(t *testing.T, home string) *Executor {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", home)
	e, err := New(t.TempDir(), gate(entity.ModeAlways))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}
func execFrame(value string) *aiserverv1.GrokBotUserComputerRequestFrame {
	return &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "r", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Exec{Exec: &aiserverv1.GrokBotUserComputerExec{ServerMessageJson: value}}}
}
func collect(t *testing.T, e *Executor, f *aiserverv1.GrokBotUserComputerRequestFrame) []*aiserverv1.GrokBotUserComputerResponseFrame {
	t.Helper()
	var frames []*aiserverv1.GrokBotUserComputerResponseFrame
	err := e.Handle(context.Background(), f, false, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error { frames = append(frames, f); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return frames
}
func TestRootConfinement(t *testing.T) {
	e := setup(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(e.Root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../secret", secret, "escape/secret"} {
		b, _ := json.Marshal(map[string]any{"id": 7, "readArgs": map[string]any{"path": path}})
		frames := collect(t, e, execFrame(string(b)))
		if len(frames) != 1 || frames[0].GetControl() == nil {
			t.Fatalf("escape accepted: %s %#v", path, frames)
		}
	}
	f := &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "w", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Upload{Upload: &aiserverv1.GrokBotUserComputerUpload{Path: "escape/new", Data: []byte("bad")}}}
	frames := collect(t, e, f)
	if frames[0].GetFileError() == nil {
		t.Fatal("symlink write accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatal("outside file created")
	}
}
func TestReadRangeAndDigest(t *testing.T) {
	e := setup(t)
	os.WriteFile(filepath.Join(e.Root, "f"), []byte("one\ntwo\nthree\n"), 0600)
	frames := collect(t, e, execFrame(`{"id":42,"readArgs":{"path":"f","offset":2,"limit":1}}`))
	if len(frames) != 2 || frames[0].GetClient() == nil {
		t.Fatal(frames)
	}
	var v struct {
		ID         int
		ReadResult struct {
			Success struct {
				Content      string
				TotalLines   int
				RangeApplied bool
				Sha256       string
			}
		}
	}
	if err := json.Unmarshal([]byte(frames[0].GetClient().MessageJson), &v); err != nil {
		t.Fatal(err)
	}
	if v.ID != 1 || v.ReadResult.Success.Content != "two" || v.ReadResult.Success.TotalLines != 4 || !v.ReadResult.Success.RangeApplied || v.ReadResult.Success.Sha256 != "" {
		t.Fatalf("%+v", v)
	}
}
func TestUploadDownload(t *testing.T) {
	e := setup(t)
	data := []byte(strings.Repeat("x", (1<<20)+19))
	collect(t, e, &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "u", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Upload{Upload: &aiserverv1.GrokBotUserComputerUpload{Path: "new", Data: data}}})
	frames := collect(t, e, &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "d", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Download{Download: &aiserverv1.GrokBotUserComputerDownload{Path: "new"}}})
	var got []byte
	for i, f := range frames {
		if f.GetFile() == nil || f.GetFile().Seq != uint32(i) {
			t.Fatal(f)
		}
		got = append(got, f.GetFile().Data...)
	}
	if string(got) != string(data) || !frames[len(frames)-1].GetFile().Last {
		t.Fatal("bad download")
	}
}
func TestAskWithoutTTYAndServerAuthorization(t *testing.T) {
	e := setup(t)
	e.gate = gate("ask")
	f := execFrame(`{"id":9,"readArgs":{"path":"missing"}}`)
	frames := collect(t, e, f)
	if !strings.Contains(frames[0].GetControl().MessageJson, "not approved") {
		t.Fatal(frames)
	}
	e.gate = gate("always")
	var got []*aiserverv1.GrokBotUserComputerResponseFrame
	e.Handle(context.Background(), f, true, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error { got = append(got, f); return nil })
	if !strings.Contains(got[0].GetControl().MessageJson, "not approved") {
		t.Fatal("server authorization ignored")
	}
}

func TestListAndUnsupported(t *testing.T) {
	e := setup(t)
	if err := os.Mkdir(filepath.Join(e.Root, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(e.Root, "child", "note.txt"), []byte("hi"), 0600)
	os.WriteFile(filepath.Join(e.Root, "skip.log"), nil, 0600)
	frames := collect(t, e, execFrame(`{"id":2,"lsArgs":{"path":".","ignore":["*.log"]}}`))
	if frames[0].GetClient() == nil {
		t.Fatal(frames)
	}
	s := frames[0].GetClient().MessageJson
	if !strings.Contains(s, "note.txt") || strings.Contains(s, "skip.log") {
		t.Fatal(s)
	}
	frames = collect(t, e, execFrame(`{"id":3,"writeArgs":{"path":"x"}}`))
	if frames[0].GetControl() == nil || !strings.Contains(frames[0].GetControl().MessageJson, "No handler found for server message of type writeArgs") {
		t.Fatal("unsupported request accepted")
	}
}
func TestListBudgetCountsOverflow(t *testing.T) {
	e := setup(t)
	for i := range 500 {
		if err := os.WriteFile(filepath.Join(e.Root, "file-"+strconv.Itoa(i)+".txt"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	frames := collect(t, e, execFrame(`{"lsArgs":{"path":"."}}`))
	tree := clientMessages(t, frames)[0].GetLsResult().GetSuccess().DirectoryTreeRoot
	if tree.ChildrenWereProcessed || len(tree.ChildrenFiles) == 0 || len(tree.ChildrenFiles)+int(tree.NumFiles) != 500 || tree.FullSubtreeExtensionCounts[".txt"] != tree.NumFiles {
		t.Fatal(tree)
	}
}

// Opening a FIFO must not block until a writer appears. Like the desktop,
// read rejects it by its type as an invalid file.
func TestFIFORejectedPromptly(t *testing.T) {
	e := setup(t)
	fifo := filepath.Join(e.Root, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	frames := collect(t, e, execFrame(`{"readArgs":{"path":"fifo"}}`))
	if reason := clientMessages(t, frames)[0].GetReadResult().GetInvalidFile().GetReason(); reason != "Path is neither a file nor a directory" {
		t.Fatal(frames)
	}
	frames = collect(t, e, &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "d", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Download{Download: &aiserverv1.GrokBotUserComputerDownload{Path: "fifo"}}})
	if !strings.Contains(frames[0].GetFileError().GetError(), "expected regular file") {
		t.Fatal(frames)
	}
	if time.Since(start) > time.Second {
		t.Fatal("FIFO blocked")
	}
}

// A terminal file read whole keeps two characters more than max_chars, like
// the desktop's terminal reader; ranges are not limited.
// Invariant 21, Go+: the desktop has no terminal-file branch in local-exec
// read and reads <root>/terminals/<n>.txt as plain text (rnd-v2 Q1). Pinned
// until the branch is removed.
// D13: like the desktop's local-exec, a terminal file is read like any other
// file.
func TestTerminalReadLikeAnyFile(t *testing.T) {
	e := setup(t)
	if err := e.root.MkdirAll(terminalfile.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := terminalfile.Command{PID: 1, Cwd: "/w", Command: "x", Started: time.Unix(0, 0)}
	text := terminalfile.Header(cmd, terminalfile.Running, time.Unix(0, 0)) + "0123456789\n"
	for _, name := range []string{terminalfile.Dir + "/1000.txt", "plain.txt"} {
		if err := os.WriteFile(filepath.Join(e.Root, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(name, args string) *agentv1.ReadSuccess {
		t.Helper()
		frames := collect(t, e, execFrame(`{"id":1,"readArgs":{"path":"`+name+`",`+args+`}}`))
		return clientMessages(t, frames)[0].GetReadResult().GetSuccess()
	}
	for _, args := range []string{`"maxChars":5`, `"maxChars":5,"offset":1`, `"maxChars":1000`} {
		terminal, plain := read(terminalfile.Dir+"/1000.txt", args), read("plain.txt", args)
		if terminal.GetContent() != plain.GetContent() || terminal.Truncated != plain.Truncated {
			t.Fatalf("%s: %q %v, plain %q %v", args, terminal.GetContent(), terminal.Truncated, plain.GetContent(), plain.Truncated)
		}
	}
}

func TestUnrecognizedFrames(t *testing.T) {
	e := setup(t)
	e.gate = gate(entity.ModeNever) // answered before any permission check
	for _, f := range []*aiserverv1.GrokBotUserComputerRequestFrame{
		{RequestId: "r"},
		{RequestId: "r", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_PutFileToUrl{PutFileToUrl: &aiserverv1.GrokBotUserComputerPutFileToUrl{Path: "x", Url: "https://example.invalid/"}}},
		{RequestId: "r", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_GetFileFromUrl{GetFileFromUrl: &aiserverv1.GrokBotUserComputerGetFileFromUrl{Path: "x", Url: "https://example.invalid/"}}},
	} {
		if !frames.Unrecognized(f) {
			t.Fatal("recognized", f)
		}
		out := collect(t, e, f)
		var msg agentv1.ExecClientControlMessage
		if len(out) != 1 || out[0].GetControl() == nil || protojson.Unmarshal([]byte(out[0].GetControl().MessageJson), &msg) != nil ||
			msg.GetThrow().GetId() != 0 || msg.GetThrow().GetError() != frames.ErrUnknownFrame.Error() {
			t.Fatal(out)
		}
	}
	f := &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "r", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool{DiscoverAiTool: &aiserverv1.GrokBotUserComputerDiscoverAiTool{Source: "cursor"}}}
	if out := collect(t, e, f); !frames.Unrecognized(f) || len(out) != 1 || out[0].GetFileError().GetError() != frames.ErrUnknownFrame.Error() {
		t.Fatal(out)
	}
	f.GetDiscoverAiTool().Source = frames.SourceCodex
	if frames.Unrecognized(f) || frames.Unrecognized(execFrame(`{}`)) {
		t.Fatal("known frame unrecognized")
	}
}

// A server-supplied path or working directory must not be able to fake a
// different action in the permission prompt (bug 1 of the 2026-10-31 review).
// L13: like the desktop's outlivesScope, the prompt says when a command can
// outlive its request.
func TestDescribeQuotesServerStrings(t *testing.T) {
	const outlives = "; it can keep running after this request ends"
	for in, want := range map[string]string{
		`{"shellStreamArgs":{"command":"ls","workingDirectory":"/tmp\r\u001b[2KRun \"true\" in /home"}}`: `Run "ls" in "/tmp\r\x1b[2KRun \"true\" in /home"`,
		`{"shellStreamArgs":{"command":"sleep 9","isBackground":true}}`:                                  `Run "sleep 9" in ""` + outlives,
		`{"backgroundShellSpawnArgs":{"command":"ls","workingDirectory":"/a\nb"}}`:                       `Run background "ls" in "/a\nb"` + outlives,
		`{"readArgs":{"path":"/a\r\u001b[2KRead /b"}}`:                                                   `Read "/a\r\x1b[2KRead /b"`,
		`{"lsArgs":{"path":"/a\u202eb"}}`:                                                                `List "/a\u202eb"`,
	} {
		msg := &agentv1.ExecServerMessage{}
		if err := protojson.Unmarshal([]byte(in), msg); err != nil {
			t.Fatal(err)
		}
		if got := describe(msg); got != want {
			t.Errorf("describe(%s) = %s, want %s", in, got, want)
		}
	}
}

// Discovery asks for permission like any other request.
func TestDiscoveryNeedsPermission(t *testing.T) {
	e := setup(t)
	e.gate = gate("ask")
	f := &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "d", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_DiscoverAiTool{DiscoverAiTool: &aiserverv1.GrokBotUserComputerDiscoverAiTool{Source: "codex"}}}
	if frames := collect(t, e, f); len(frames) != 1 || frames[0].GetFileError() == nil {
		t.Fatal("non-TTY discovery allowed")
	}
}

// L2-1: like the desktop (ult, q9), a root given through a symlink is kept
// as it is, in the hello and for relative working directories; a path is
// checked lexically against it, so the symlink's target is outside it.
func TestRootThroughSymlink(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "001")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "d"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hello"), 0o600))
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(dir, "escape")))
	require.NoError(t, os.Mkdir(filepath.Join(base, "002"), 0o700))
	link := filepath.Join(base, "002", "home")
	require.NoError(t, os.Symlink(dir, link))
	t.Setenv("HOME", link)
	t.Setenv("ZDOTDIR", t.TempDir())
	e, err := New(link, gate(entity.ModeAlways))
	require.NoError(t, err)
	defer e.Close()
	assert.Equal(t, link, e.Root)
	assert.Equal(t, filepath.Join(link, terminalfile.Dir), e.TerminalsFolder())

	frames := pinnedExec(t, e, `{"readArgs":{"path":"`+link+`/f.txt"}}`)
	require.NotEmpty(t, frames)
	assert.Equal(t, "hello", frames[0].client.GetReadResult().GetSuccess().GetContent())
	text, _ := requireThrow(t, pinnedExec(t, e, `{"readArgs":{"path":"`+dir+`/f.txt"}}`))
	assert.Equal(t, "Path is outside the allowed local-exec root and was refused: "+dir+"/f.txt", text)
	text, _ = requireThrow(t, pinnedExec(t, e, `{"lsArgs":{"path":"`+link+`/escape"}}`))
	assert.Equal(t, "Path resolves through a symlink to outside the allowed local-exec root and was refused: "+link+"/escape", text)

	// The shell starts in the directory as given, and its pwd says so.
	r := runShell(t, e, "pwd")
	assert.Equal(t, link+"\n", r.stdout, r.stderr)
	assert.Equal(t, link, r.exit.GetCwd())
	r = runShellFrame(t, e, `{"shellStreamArgs":{"command":"pwd","workingDirectory":"d"}}`)
	assert.Equal(t, filepath.Join(link, "d")+"\n", r.stdout, r.stderr)
	frames = pinnedExec(t, e, `{"backgroundShellSpawnArgs":{"command":"true","workingDirectory":"d"}}`)
	require.NotEmpty(t, frames)
	assert.Equal(t, filepath.Join(link, "d"), frames[0].client.GetBackgroundShellSpawnResult().GetSuccess().GetWorkingDirectory())
	r = runShellFrame(t, e, `{"shellStreamArgs":{"command":"true","workingDirectory":"missing"}}`)
	assert.Equal(t, "working directory missing does not exist on this machine; running in "+link+" instead\n", r.stderr)
	r = runShell(t, e, "cd ~ && pwd")
	assert.Equal(t, link+"\n", r.stdout, r.stderr)
	assert.Equal(t, link, r.exit.GetCwd())
}
