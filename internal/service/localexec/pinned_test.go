package localexec

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

// Tests in this file pin current behavior before the refactor. Invariant
// numbers refer to ai.tmp/refactor-plan/grok-bot-map.md §4; "≠" marks a known
// difference from the desktop that a later alignment PR changes on purpose.

// pinnedFrame is a response frame decoded for assertions.
type pinnedFrame struct {
	control  *agentv1.ExecClientControlMessage
	client   *agentv1.ExecClientMessage
	cwdState *string
	file     *aiserverv1.GrokBotUserComputerFileError
}

func pinnedFrames(ctx context.Context, t *testing.T, e *Executor, f *aiserverv1.GrokBotUserComputerRequestFrame) []pinnedFrame {
	t.Helper()
	var out []pinnedFrame
	err := e.Handle(ctx, f, false, func(r *aiserverv1.GrokBotUserComputerResponseFrame) error {
		var p pinnedFrame
		switch {
		case r.GetControl() != nil:
			p.control = &agentv1.ExecClientControlMessage{}
			require.NoError(t, protojson.Unmarshal([]byte(r.GetControl().MessageJson), p.control))
			p.cwdState = r.GetControl().CwdState
		case r.GetClient() != nil:
			p.client = &agentv1.ExecClientMessage{}
			require.NoError(t, protojson.Unmarshal([]byte(r.GetClient().MessageJson), p.client))
		case r.GetFileError() != nil:
			p.file = r.GetFileError()
		}
		out = append(out, p)
		return nil
	})
	require.NoError(t, err)
	return out
}

func pinnedExec(t *testing.T, e *Executor, msg string) []pinnedFrame {
	t.Helper()
	return pinnedFrames(context.Background(), t, e, execFrame(msg))
}

// requireThrow asserts that frames are a single exec throw, without a
// streamClose, and returns its error text.
func requireThrow(t *testing.T, frames []pinnedFrame) (string, *string) {
	t.Helper()
	require.Len(t, frames, 1)
	throw := frames[0].control.GetThrow()
	require.NotNil(t, throw, "want throw, got %+v", frames[0])
	assert.NotZero(t, throw.GetId())
	return throw.GetError(), frames[0].cwdState
}

func shellEvents(frames []pinnedFrame) []*agentv1.ShellStream {
	var out []*agentv1.ShellStream
	for _, f := range frames {
		if s := f.client.GetShellStream(); s != nil {
			out = append(out, s)
		}
	}
	return out
}

// Invariant 20 (D5, aligned with the desktop). A read or ls outside Root is
// one exec throw without streamClose, with cwd_state "unset", the desktop's
// text and the first line of its stack trace; upload and download answer
// with a fileError of the same text (rnd-v2/SUMMARY.md, Q2). L10: a
// dangling symlink out of Root, which the desktop's check lets through and
// os.Root refuses, is refused in the same words; ≠: the desktop would follow
// it (stricter on purpose, rnd-v2/SUMMARY.md).
func TestPinnedRootEscape(t *testing.T) {
	e := setup(t)
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(e.Root, "escape")))
	gone := filepath.Join(t.TempDir(), "gone")
	require.NoError(t, os.Symlink(gone, filepath.Join(e.Root, "dangling")))
	const (
		outside = "Path is outside the allowed local-exec root and was refused: "
		symlink = "Path resolves through a symlink to outside the allowed local-exec root and was refused: "
	)
	for _, tc := range []struct{ msg, want string }{
		{`{"readArgs":{"path":"../x"}}`, outside + "../x"},
		{`{"readArgs":{"path":"/"}}`, outside + "/"},
		{`{"lsArgs":{"path":"../"}}`, outside + "../"},
		{`{"readArgs":{"path":"escape/x"}}`, symlink + "escape/x"},
		{`{"lsArgs":{"path":"escape"}}`, symlink + "escape"},
		{`{"readArgs":{"path":"dangling"}}`, symlink + "dangling"},
		{`{"lsArgs":{"path":"dangling"}}`, symlink + "dangling"},
	} {
		frames := pinnedExec(t, e, tc.msg)
		text, cwd := requireThrow(t, frames)
		assert.Equal(t, tc.want, text, tc.msg)
		assert.Equal(t, "SandLocalExecPathError: "+tc.want, frames[0].control.GetThrow().GetStackTrace(), tc.msg)
		if assert.NotNil(t, cwd, tc.msg) {
			assert.Equal(t, "unset", *cwd, tc.msg)
		}
	}
	upload := &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "u", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Upload{
		Upload: &aiserverv1.GrokBotUserComputerUpload{Path: "../x", Data: []byte("x")},
	}}
	download := &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "d", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Download{
		Download: &aiserverv1.GrokBotUserComputerDownload{Path: "escape/x"},
	}}
	downloadDangling := &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "dd", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Download{
		Download: &aiserverv1.GrokBotUserComputerDownload{Path: "dangling"},
	}}
	uploadDangling := &aiserverv1.GrokBotUserComputerRequestFrame{RequestId: "ud", Frame: &aiserverv1.GrokBotUserComputerRequestFrame_Upload{
		Upload: &aiserverv1.GrokBotUserComputerUpload{Path: "dangling/x", Data: []byte("x")},
	}}
	for f, want := range map[*aiserverv1.GrokBotUserComputerRequestFrame]string{
		upload:           outside + "../x",
		download:         symlink + "escape/x",
		downloadDangling: symlink + "dangling",
		uploadDangling:   symlink + "dangling/x",
	} {
		frames := pinnedFrames(context.Background(), t, e, f)
		require.Len(t, frames, 1)
		require.NotNil(t, frames[0].file, f.RequestId)
		assert.Equal(t, want, frames[0].file.GetError(), f.RequestId)
	}
}

// Invariant 20 (D4, aligned with the desktop): a shell or background command
// runs in any existing directory, also outside Root.
func TestPinnedShellCwdOutsideRoot(t *testing.T) {
	e := setup(t)
	events := shellEvents(pinnedExec(t, e, `{"shellStreamArgs":{"command":"pwd","workingDirectory":"/"}}`))
	require.GreaterOrEqual(t, len(events), 3)
	assert.Equal(t, "/\n", events[1].GetStdout().GetData())
	assert.Equal(t, "/", events[len(events)-1].GetExit().GetCwd())
	frames := pinnedExec(t, e, `{"backgroundShellSpawnArgs":{"command":"pwd","workingDirectory":"/"}}`)
	require.Len(t, frames, 2)
	assert.Equal(t, "/", frames[0].client.GetBackgroundShellSpawnResult().GetSuccess().GetWorkingDirectory())
}

// Invariant 18. cwd_state of a failed shell request says whether its working
// directory exists, as the desktop checks it, also outside Root; other
// requests report "unset". A $SHELL that cannot be executed makes the shell
// fail after the directory was accepted. L9 (aligned with the desktop): a
// missing directory or a file was replaced by Root before the command ran,
// so the state is that of Root, "exists".
func TestPinnedCwdState(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "bash")
	require.NoError(t, os.WriteFile(broken, []byte("\x00not a program"), 0o700))
	t.Setenv("SHELL", broken)
	e := setup(t)
	require.NoError(t, os.WriteFile(filepath.Join(e.Root, "file"), nil, 0o600))
	for msg, want := range map[string]string{
		`{"shellStreamArgs":{"command":"pwd"}}`:                                    "exists",
		`{"shellStreamArgs":{"command":"pwd","workingDirectory":"nope"}}`:          "exists",
		`{"shellStreamArgs":{"command":"pwd","workingDirectory":"file"}}`:          "exists",
		`{"shellStreamArgs":{"command":"pwd","workingDirectory":"/"}}`:             "exists",
		`{"readArgs":{"path":"nope/../../x"}}`:                                     "unset",
		`{"backgroundShellSpawnArgs":{"command":"pwd","workingDirectory":"/"}}`:    "exists",
		`{"backgroundShellSpawnArgs":{"command":"pwd","workingDirectory":"nope"}}`: "exists",
	} {
		frames := pinnedExec(t, e, msg)
		last := frames[len(frames)-1]
		require.NotNil(t, last.control.GetThrow(), msg)
		if assert.NotNil(t, last.cwdState, msg) {
			assert.Equal(t, want, *last.cwdState, msg)
		}
	}
}

// L12 (aligned with the desktop, which sends err.stack with every throw of
// a tool): a tool's throw carries a stack trace. ≠: it is only the first
// line of a JavaScript stack, "Error: <message>" (or the error's own, see
// TestPinnedRootEscape); the "    at ..." frames have no counterpart here.
func TestPinnedToolThrowStackTrace(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "bash")
	require.NoError(t, os.WriteFile(broken, []byte("\x00not a program"), 0o700))
	t.Setenv("SHELL", broken)
	e := setup(t)
	frames := pinnedExec(t, e, `{"backgroundShellSpawnArgs":{"command":"pwd"}}`)
	throw := frames[len(frames)-1].control.GetThrow()
	require.NotNil(t, throw)
	assert.Equal(t, "Error: "+throw.GetError(), throw.GetStackTrace())
}

// Invariant 19. A missing foreground working directory is reported on stderr
// before start, and the command runs in Root.
func TestPinnedForegroundMissingCwd(t *testing.T) {
	e := setup(t)
	events := shellEvents(pinnedExec(t, e, `{"shellStreamArgs":{"command":"pwd","workingDirectory":"nope"}}`))
	require.GreaterOrEqual(t, len(events), 3)
	assert.Equal(t, "working directory nope does not exist on this machine; running in "+e.Root+" instead\n",
		events[0].GetStderr().GetData())
	assert.NotNil(t, events[1].GetStart())
	assert.Equal(t, e.Root+"\n", events[2].GetStdout().GetData())
}

// Invariant 33 (D1, D2, aligned with the desktop, rnd-v2 Q3). The soft
// timeout is the given one, else 0 for isBackground, BACKGROUND or a hard
// timeout, else 30 s. Without BACKGROUND it aborts the command, at once if 0;
// with BACKGROUND it moves the command to the background: start, output,
// backgrounded{shellId, pid, msToWait, reason TIMEOUT}, streamClose, with the
// terminal file created then. A process killed by a signal reports exit code
// 4294967295.
func TestPinnedShellTimeouts(t *testing.T) {
	const killed = uint32(4294967295)
	run := func(t *testing.T, e *Executor, args string) []pinnedFrame {
		t.Helper()
		return pinnedExec(t, e, `{"shellStreamArgs":{`+args+`}}`)
	}
	t.Run("default timeout", func(t *testing.T) {
		events := shellEvents(run(t, setup(t), `"command":"sleep 0.2; echo done"`))
		exit := events[len(events)-1].GetExit()
		require.NotNil(t, exit)
		assert.False(t, exit.GetAborted())
	})
	t.Run("timeout aborts", func(t *testing.T) {
		events := shellEvents(run(t, setup(t), `"command":"sleep 5","timeout":100`))
		exit := events[len(events)-1].GetExit()
		require.NotNil(t, exit)
		assert.True(t, exit.GetAborted())
		assert.Equal(t, agentv1.ShellAbortReason_SHELL_ABORT_REASON_TIMEOUT, exit.GetAbortReason())
		assert.Equal(t, killed, exit.GetCode())
	})
	t.Run("isBackground without BACKGROUND aborts at once", func(t *testing.T) {
		start := time.Now()
		events := shellEvents(run(t, setup(t), `"command":"sleep 5","isBackground":true`))
		assert.Less(t, time.Since(start), 3*time.Second)
		require.Len(t, events, 2)
		assert.NotNil(t, events[0].GetStart())
		exit := events[1].GetExit()
		require.NotNil(t, exit)
		assert.True(t, exit.GetAborted())
		assert.Equal(t, agentv1.ShellAbortReason_SHELL_ABORT_REASON_TIMEOUT, exit.GetAbortReason())
		assert.Equal(t, killed, exit.GetCode())
	})
	t.Run("isBackground with a timeout runs in the foreground", func(t *testing.T) {
		events := shellEvents(run(t, setup(t), `"command":"echo hi","isBackground":true,"timeout":5000`))
		assert.Equal(t, "hi\n", events[1].GetStdout().GetData())
		exit := events[len(events)-1].GetExit()
		require.NotNil(t, exit)
		assert.False(t, exit.GetAborted())
	})
	t.Run("BACKGROUND with zero timeout backgrounds at once", func(t *testing.T) {
		e := setup(t)
		frames := run(t, e, `"command":"sleep 5","timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"`)
		events := shellEvents(frames)
		require.Len(t, events, 2)
		assert.NotNil(t, events[0].GetStart())
		bg := events[1].GetBackgrounded()
		require.NotNil(t, bg)
		require.NotNil(t, bg.MsToWait)
		assert.Zero(t, bg.GetMsToWait())
		assert.Equal(t, agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT, bg.GetReason())
		assert.NotNil(t, frames[len(frames)-1].control.GetStreamClose())
		_, err := os.Stat(filepath.Join(e.Root, "terminals", fmt.Sprintf("%d.txt", bg.GetShellId())))
		require.NoError(t, err)
	})
	t.Run("BACKGROUND on timeout", func(t *testing.T) {
		e := setup(t)
		start := time.Now()
		frames := run(t, e, `"command":"echo early; sleep 5","timeout":100,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"`)
		assert.Less(t, time.Since(start), 3*time.Second)
		events := shellEvents(frames)
		require.Len(t, events, 3)
		assert.NotNil(t, events[0].GetStart())
		assert.Equal(t, "early\n", events[1].GetStdout().GetData())
		bg := events[2].GetBackgrounded()
		require.NotNil(t, bg)
		assert.Equal(t, agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT, bg.GetReason())
		assert.Equal(t, int32(100), bg.GetMsToWait())
		assert.NotZero(t, bg.GetPid())
		assert.NotNil(t, frames[len(frames)-1].control.GetStreamClose())
		data, err := os.ReadFile(filepath.Join(e.Root, "terminals", fmt.Sprintf("%d.txt", bg.GetShellId())))
		require.NoError(t, err)
		assert.Contains(t, string(data), "---\nearly\n", "output before the move missing")
		assert.NotContains(t, string(data), "running_for_ms: 0 ")
	})
	t.Run("BACKGROUND hard timeout before the move ends without exit", func(t *testing.T) {
		frames := run(t, setup(t), `"command":"sleep 5","timeout":5000,"hardTimeout":100,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"`)
		events := shellEvents(frames)
		require.Len(t, events, 1)
		assert.NotNil(t, events[0].GetStart())
		assert.NotNil(t, frames[len(frames)-1].control.GetStreamClose())
	})
	t.Run("BACKGROUND canceled before the move", func(t *testing.T) {
		// Like the desktop (S15), the stream ends at once with an exit of its
		// own: no output of the dying command, no execution time.
		e := setup(t)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		events := shellEvents(pinnedFrames(ctx, t, e, execFrame(
			`{"shellStreamArgs":{"command":"trap 'echo trapped; exit 0' TERM; sleep 5","timeout":5000,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`)))
		assert.Less(t, time.Since(start), 600*time.Millisecond)
		require.Len(t, events, 2)
		assert.NotNil(t, events[0].GetStart())
		exit := events[1].GetExit()
		require.NotNil(t, exit)
		assert.True(t, exit.GetAborted())
		assert.Equal(t, agentv1.ShellAbortReason_SHELL_ABORT_REASON_USER_ABORT, exit.GetAbortReason())
		assert.Equal(t, killed, exit.GetCode())
		assert.Equal(t, e.Root, exit.GetCwd())
		assert.Nil(t, exit.LocalExecutionTimeMs)
	})
}

// D9: like the desktop, start carries the sandbox policy of a local approval
// and no command, and exit carries an abort reason only when aborted, and
// the execution time in milliseconds (S7).
func TestPinnedShellStartAndExit(t *testing.T) {
	events := shellEvents(pinnedExec(t, setup(t), `{"shellStreamArgs":{"command":"sleep 0.2"}}`))
	start := events[0].GetStart()
	require.NotNil(t, start)
	assert.Nil(t, start.Command)
	assert.Equal(t, agentv1.SandboxPolicy_TYPE_INSECURE_NONE, start.GetSandboxPolicy().GetType())
	exit := events[len(events)-1].GetExit()
	require.NotNil(t, exit)
	assert.Nil(t, exit.AbortReason)
	require.NotNil(t, exit.LocalExecutionTimeMs)
	assert.GreaterOrEqual(t, exit.GetLocalExecutionTimeMs(), int32(200))
	assert.Less(t, exit.GetLocalExecutionTimeMs(), int32(3000))
	events = shellEvents(pinnedExec(t, setup(t), `{"shellStreamArgs":{"command":"sleep 5","timeout":100}}`))
	assert.NotNil(t, events[len(events)-1].GetExit().LocalExecutionTimeMs, "aborted")
}

// S5, aligned with the desktop's dS: without a working directory, a session
// cwd that no longer exists gives start, the desktop's stderr and exit 1 with
// that cwd; the command does not run.
func TestPinnedShellGoneCwd(t *testing.T) {
	e := setup(t)
	gone := filepath.Join(e.Root, "gone")
	pinnedExec(t, e, `{"shellStreamArgs":{"command":"mkdir gone && cd gone && rmdir ../gone"}}`)
	events := shellEvents(pinnedExec(t, e, `{"shellStreamArgs":{"command":"touch ran"}}`))
	require.Len(t, events, 3)
	assert.NotNil(t, events[0].GetStart())
	assert.Equal(t, "The shell's working directory no longer exists: "+gone+
		". The command did not run. Re-run it with working_directory set to an existing directory.\n", events[1].GetStderr().GetData())
	exit := events[2].GetExit()
	require.NotNil(t, exit)
	assert.Equal(t, uint32(1), exit.GetCode())
	assert.Equal(t, gone, exit.GetCwd())
	assert.False(t, exit.GetAborted())
	assert.Nil(t, exit.AbortReason)
	assert.NotNil(t, exit.LocalExecutionTimeMs)
	assert.NoFileExists(t, filepath.Join(e.Root, "ran"))
}

// S19 ≠: BACKGROUND commands read stdin from /dev/null; the desktop gives
// them a pipe, so "read" there waits for input that the agent cannot send
// on local-exec. EOF at once is kept on purpose.
func TestPinnedBackgroundStdin(t *testing.T) {
	e := setup(t)
	events := shellEvents(pinnedExec(t, e, `{"shellStreamArgs":{"command":"read x; echo rc=$?","timeout":5000,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`))
	require.GreaterOrEqual(t, len(events), 2)
	assert.Equal(t, "rc=1\n", events[1].GetStdout().GetData())
	assert.NotNil(t, events[len(events)-1].GetExit())
}

// D9: the desktop's read texts for an empty path and a binary file.
func TestPinnedReadTexts(t *testing.T) {
	e := setup(t)
	frames := pinnedExec(t, e, `{"readArgs":{"path":"  "}}`)
	require.NotEmpty(t, frames)
	readErr := frames[0].client.GetReadResult().GetError()
	require.NotNil(t, readErr)
	assert.Equal(t, "  ", readErr.GetPath())
	assert.Equal(t, "path is empty or only whitespace", readErr.GetError())

	require.NoError(t, os.WriteFile(filepath.Join(e.Root, "blob.bin"), []byte{0, 1, 2, 0xff, 0}, 0o600))
	frames = pinnedExec(t, e, `{"readArgs":{"path":"blob.bin"}}`)
	require.NotEmpty(t, frames)
	assert.Equal(t, "File content is binary data, not text", frames[0].client.GetReadResult().GetInvalidFile().GetReason())
}

// Like the desktop, an aborted command reports the session's cwd, not the
// directory it ran in.
func TestPinnedAbortedExitCwd(t *testing.T) {
	e := setup(t)
	dir := t.TempDir()
	events := shellEvents(pinnedExec(t, e, `{"shellStreamArgs":{"command":"sleep 5","timeout":100,"workingDirectory":"`+dir+`"}}`))
	exit := events[len(events)-1].GetExit()
	require.NotNil(t, exit)
	assert.True(t, exit.GetAborted())
	assert.Equal(t, e.Root, exit.GetCwd())
}

// Like the desktop, an aborted command gets SIGTERM first and SIGKILL a
// second later.
func TestPinnedAbortSignals(t *testing.T) {
	e := setup(t)
	start := time.Now()
	events := shellEvents(pinnedExec(t, e, `{"shellStreamArgs":{"command":"trap 'echo term; trap - TERM' TERM; while :; do sleep 0.05; done","timeout":200}}`))
	assert.GreaterOrEqual(t, time.Since(start), time.Second, "SIGKILL came before the delay")
	var out strings.Builder
	for _, ev := range events {
		out.WriteString(ev.GetStdout().GetData())
	}
	assert.Contains(t, out.String(), "term")
	exit := events[len(events)-1].GetExit()
	require.NotNil(t, exit)
	assert.Equal(t, uint32(4294967295), exit.GetCode())
}

// largeWebP is a WEBP sample with its size set to w×h in its header: the
// lossless one decodes at any size (its pixels take no bits), the animated
// one does not decode in Go.
func largeWebP(t *testing.T, animated bool, w, h int) []byte {
	t.Helper()
	sample := "UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA=="
	if animated {
		sample = "UklGRlIAAABXRUJQVlA4WAoAAAASAAAAAAAAAAAAQU5JTQYAAAD/////AABBTk1GJgAAAAAAAAAAAAAAAAAAAGQAAABWUDhMDQAAAC8AAAAQBxAREYiI/gcA"
	}
	b, err := base64.StdEncoding.DecodeString(sample)
	require.NoError(t, err)
	if animated { // VP8X canvas, 24 bits each, minus one
		w, h = w-1, h-1
		copy(b[24:30], []byte{byte(w), byte(w >> 8), byte(w >> 16), byte(h), byte(h >> 8), byte(h >> 16)})
		return b
	}
	v := uint32(w-1) | uint32(h-1)<<14 | uint32(b[24]&0xe0)<<24 //nolint:gosec // test sizes
	copy(b[21:25], []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
	return b
}

// ≠: the desktop 0.78.1 does not decode a WEBP larger than 1280 pixels or
// 1 MiB and answers with an error, "webp codec not registered…"; we encode
// it again as JPEG, or PNG when it has transparency. One that Go cannot
// decode either (animated) gets the desktop's error.
func TestPinnedReadLargeWebP(t *testing.T) {
	e := setup(t)
	require.NoError(t, os.WriteFile(filepath.Join(e.Root, "large.webp"), largeWebP(t, false, 2000, 1500), 0o600))
	frames := pinnedExec(t, e, `{"readArgs":{"path":"large.webp"}}`)
	require.NotEmpty(t, frames)
	data := frames[0].client.GetReadResult().GetSuccess().GetData()
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, "png", format, "transparent pixels")
	assert.Equal(t, image.Pt(1280, 960), image.Pt(cfg.Width, cfg.Height))

	require.NoError(t, os.WriteFile(filepath.Join(e.Root, "anim.webp"), largeWebP(t, true, 2000, 1500), 0o600))
	frames = pinnedExec(t, e, `{"readArgs":{"path":"anim.webp"}}`)
	require.NotEmpty(t, frames)
	assert.Equal(t, "webp codec not registered: registerWebpCodec() must be called by a Node server before decoding/encoding webp",
		frames[0].client.GetReadResult().GetError().GetError())
}
