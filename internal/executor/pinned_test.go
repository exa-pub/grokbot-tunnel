package executor

import (
	"context"
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

// Invariant 20, ≠ texts. A read or ls outside Root is one exec throw without
// streamClose, as on the desktop, and its cwd_state is "unset". The desktop
// says "Path is outside the allowed local-exec root and was refused: <p>" and
// "Path resolves through a symlink to outside the allowed local-exec root and
// was refused: <p>", adds a stackTrace, and answers upload and download with
// those texts too (rnd-v2/SUMMARY.md, Q2).
func TestPinnedRootEscape(t *testing.T) {
	e := setup(t)
	require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(e.Root, "escape")))
	for _, tc := range []struct{ msg, want string }{
		{`{"readArgs":{"path":"../x"}}`, "path is outside the allowed local-exec root"},
		{`{"readArgs":{"path":"/"}}`, "path is outside the allowed local-exec root"},
		{`{"lsArgs":{"path":"../"}}`, "path is outside the allowed local-exec root"},
		{`{"readArgs":{"path":"escape/x"}}`, "statat escape/x: path escapes from parent"},
		{`{"lsArgs":{"path":"escape"}}`, "statat escape: path escapes from parent"},
	} {
		text, cwd := requireThrow(t, pinnedExec(t, e, tc.msg))
		assert.Equal(t, tc.want, text, tc.msg)
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
		upload:   "path is outside the allowed local-exec root",
		download: "openat escape/x: path escapes from parent",
	} {
		frames := pinnedFrames(context.Background(), t, e, f)
		require.Len(t, frames, 1)
		require.NotNil(t, frames[0].file, f.RequestId)
		assert.Equal(t, want, frames[0].file.GetError(), f.RequestId)
	}
}

// Invariant 20, ≠ cwd (decided: align with the desktop). A shell or
// background working directory outside Root is refused with a throw without
// cwd_state. The desktop runs the command in any existing directory.
func TestPinnedShellCwdOutsideRoot(t *testing.T) {
	e := setup(t)
	for _, msg := range []string{
		`{"shellStreamArgs":{"command":"pwd","workingDirectory":"/"}}`,
		`{"backgroundShellSpawnArgs":{"command":"pwd","workingDirectory":"/"}}`,
	} {
		text, cwd := requireThrow(t, pinnedExec(t, e, msg))
		assert.Equal(t, "path is outside the allowed local-exec root", text, msg)
		assert.Nil(t, cwd, msg)
	}
}

// Invariant 18. cwd_state of a failed shell request reports whether its
// working directory exists; other requests report "unset", and a directory
// that cannot be checked leaves the field out. A $SHELL that cannot be
// executed makes the shell fail after the directory was accepted.
func TestPinnedCwdState(t *testing.T) {
	broken := filepath.Join(t.TempDir(), "bash")
	require.NoError(t, os.WriteFile(broken, []byte("\x00not a program"), 0o700))
	t.Setenv("SHELL", broken)
	e := setup(t)
	require.NoError(t, os.WriteFile(filepath.Join(e.Root, "file"), nil, 0o600))
	for msg, want := range map[string]string{
		`{"shellStreamArgs":{"command":"pwd"}}`:                                 "exists",
		`{"shellStreamArgs":{"command":"pwd","workingDirectory":"nope"}}`:       "missing",
		`{"shellStreamArgs":{"command":"pwd","workingDirectory":"file"}}`:       "",
		`{"shellStreamArgs":{"command":"pwd","workingDirectory":"/"}}`:          "",
		`{"readArgs":{"path":"nope/../../x"}}`:                                  "unset",
		`{"backgroundShellSpawnArgs":{"command":"pwd","workingDirectory":"/"}}`: "",
	} {
		frames := pinnedExec(t, e, msg)
		last := frames[len(frames)-1]
		require.NotNil(t, last.control.GetThrow(), msg)
		if want == "" {
			assert.Nil(t, last.cwdState, msg)
		} else if assert.NotNil(t, last.cwdState, msg) {
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

// Invariant 33, ≠. Pins how timeouts and isBackground behave today; the
// desktop's sequence is in rnd-v2/q3 (both models) and replaces these
// expectations in the alignment PR:
//   - timeout 0 without isBackground: no timeout here, 30 s soft timeout there;
//   - abort: exit code 137 here, 4294967295 ("unknown") there;
//   - isBackground: immediate backgrounded without start or reason here; there
//     it only zeroes the default timeout, and without BACKGROUND a zero
//     timeout aborts at once;
//   - BACKGROUND with timeout 0: runs to the end here, backgrounds at once
//     there;
//   - BACKGROUND on timeout: start, output, backgrounded{reason TIMEOUT},
//     streamClose in both; the desktop also sets msToWait.
func TestPinnedShellTimeouts(t *testing.T) {
	e := setup(t)
	t.Run("zero timeout", func(t *testing.T) {
		events := shellEvents(pinnedExec(t, e, `{"shellStreamArgs":{"command":"sleep 0.2; echo done"}}`))
		exit := events[len(events)-1].GetExit()
		require.NotNil(t, exit)
		assert.False(t, exit.GetAborted())
	})
	t.Run("abort code", func(t *testing.T) {
		events := shellEvents(pinnedExec(t, e, `{"shellStreamArgs":{"command":"sleep 5","timeout":100}}`))
		exit := events[len(events)-1].GetExit()
		require.NotNil(t, exit)
		assert.True(t, exit.GetAborted())
		assert.Equal(t, agentv1.ShellAbortReason_SHELL_ABORT_REASON_TIMEOUT, exit.GetAbortReason())
		assert.Equal(t, uint32(137), exit.GetCode())
	})
	t.Run("isBackground", func(t *testing.T) {
		frames := pinnedExec(t, e, `{"shellStreamArgs":{"command":"echo hi","isBackground":true}}`)
		require.Len(t, frames, 2)
		bg := frames[0].client.GetShellStream().GetBackgrounded()
		require.NotNil(t, bg)
		assert.NotZero(t, bg.GetShellId())
		assert.Nil(t, bg.Reason)
		assert.Nil(t, bg.MsToWait)
		assert.NotNil(t, frames[1].control.GetStreamClose())
	})
	t.Run("background behavior with zero timeout", func(t *testing.T) {
		events := shellEvents(pinnedExec(t, e,
			`{"shellStreamArgs":{"command":"echo done","timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`))
		assert.NotNil(t, events[len(events)-1].GetExit())
	})
	t.Run("background on timeout", func(t *testing.T) {
		start := time.Now()
		frames := pinnedExec(t, e, `{"shellStreamArgs":{"command":"echo early; sleep 5",`+
			`"timeout":100,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`)
		assert.Less(t, time.Since(start), 3*time.Second)
		events := shellEvents(frames)
		require.Len(t, events, 3)
		assert.NotNil(t, events[0].GetStart())
		assert.Equal(t, "early\n", events[1].GetStdout().GetData())
		bg := events[2].GetBackgrounded()
		require.NotNil(t, bg)
		assert.Equal(t, agentv1.ShellBackgroundReason_SHELL_BACKGROUND_REASON_TIMEOUT, bg.GetReason())
		assert.Nil(t, bg.MsToWait)
		assert.NotNil(t, frames[len(frames)-1].control.GetStreamClose())
	})
}
