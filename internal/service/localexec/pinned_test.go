package localexec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
// with a fileError of the same text (rnd-v2/SUMMARY.md, Q2).
func TestPinnedRootEscape(t *testing.T) {
	e := setup(t)
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(e.Root, "escape")))
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
	for f, want := range map[*aiserverv1.GrokBotUserComputerRequestFrame]string{
		upload:   outside + "../x",
		download: symlink + "escape/x",
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
// fail after the directory was accepted.
func TestPinnedCwdState(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "bash")
	require.NoError(t, os.WriteFile(broken, []byte("\x00not a program"), 0o700))
	t.Setenv("SHELL", broken)
	e := setup(t)
	require.NoError(t, os.WriteFile(filepath.Join(e.Root, "file"), nil, 0o600))
	for msg, want := range map[string]string{
		`{"shellStreamArgs":{"command":"pwd"}}`:                                 "exists",
		`{"shellStreamArgs":{"command":"pwd","workingDirectory":"nope"}}`:       "missing",
		`{"shellStreamArgs":{"command":"pwd","workingDirectory":"file"}}`:       "missing",
		`{"shellStreamArgs":{"command":"pwd","workingDirectory":"/"}}`:          "exists",
		`{"readArgs":{"path":"nope/../../x"}}`:                                  "unset",
		`{"backgroundShellSpawnArgs":{"command":"pwd","workingDirectory":"/"}}`: "exists",
	} {
		frames := pinnedExec(t, e, msg)
		last := frames[len(frames)-1]
		require.NotNil(t, last.control.GetThrow(), msg)
		if assert.NotNil(t, last.cwdState, msg) {
			assert.Equal(t, want, *last.cwdState, msg)
		}
	}
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
		e := setup(t)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		events := shellEvents(pinnedFrames(ctx, t, e, execFrame(
			`{"shellStreamArgs":{"command":"sleep 5","timeout":5000,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`)))
		exit := events[len(events)-1].GetExit()
		require.NotNil(t, exit)
		assert.True(t, exit.GetAborted())
		assert.Equal(t, agentv1.ShellAbortReason_SHELL_ABORT_REASON_USER_ABORT, exit.GetAbortReason())
		assert.Equal(t, killed, exit.GetCode())
	})
}
