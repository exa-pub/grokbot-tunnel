package localexec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
)

func TestBackgroundReplaysForegroundState(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		runShell(t, e, "grok_bg_fn() { printf bg-fn-ok; }; export BG_VAR=1")
		data := waitTerminal(t, e, collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"grok_bg_fn; export BG_VAR=2"}}`)))
		if !strings.Contains(data, "bg-fn-ok") || strings.Count(data, "exit_code:") != 1 {
			t.Fatalf("%q", data)
		}
		// D3: like the desktop, a background command that ends on its own
		// writes its state back to the session (rnd-v2/q4-background-state.md).
		expectStdout(t, e, `printf %s "$BG_VAR"`, "2")
	})
}

// Background jobs of a foreground command outlive it: its pipes are drained,
// not closed. Output that arrives up to 5 s after the command exits is still
// forwarded, before the exit, like the desktop's closeTimeout.
func TestShellBackgroundJobSurvivesCommand(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, "(sleep 0.2; echo late) &", "late\n")
		r := runShell(t, e, "(sleep 1.5; echo later; echo later >&2) &")
		if r.stdout != "later\n" || r.stderr != "later\n" {
			t.Fatal(r.stdout, r.stderr)
		}
	})
	t.Setenv("SHELL", "/bin/bash")
	e := setup(t)
	start := time.Now()
	runShell(t, e, "(sleep 6; echo alive > marker) &")
	if d := time.Since(start); d < 4900*time.Millisecond || d > 5900*time.Millisecond {
		t.Fatal("command did not wait 5 s for its background job:", d)
	}
	marker := filepath.Join(e.Root, "marker")
	deadline := time.Now().Add(4 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background job was killed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// S2-1: like the desktop's io(), which stops listening for the abort once the
// shell exits, an abort or timeout after the shell has exited, while its
// background jobs still hold the pipes, kills nothing: the exit comes once
// the output is finished, as aborted, and the jobs keep running.
func TestShellAbortAfterShellExitKeepsJobs(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	const job = `(sleep 1.2; echo alive > marker) & `
	for _, c := range []struct {
		name, msg string
		cancel    time.Duration // 0: no cancel
		reason    agentv1.ShellAbortReason
		backgr    bool // the command moves to the background first
	}{
		{name: "timeout", msg: `{"shellStreamArgs":{"command":"` + job + `echo started","timeout":400}}`,
			reason: agentv1.ShellAbortReason_SHELL_ABORT_REASON_TIMEOUT},
		{name: "cancel", msg: `{"shellStreamArgs":{"command":"` + job + `echo started"}}`, cancel: 400 * time.Millisecond,
			reason: agentv1.ShellAbortReason_SHELL_ABORT_REASON_USER_ABORT},
		{name: "cancel-background", msg: `{"shellStreamArgs":{"command":"` + job + `echo started","timeout":5000,` +
			`"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`, cancel: 400 * time.Millisecond,
			reason: agentv1.ShellAbortReason_SHELL_ABORT_REASON_USER_ABORT},
		{name: "hard-timeout-detached", msg: `{"shellStreamArgs":{"command":"` + job + `sleep 0.2","timeout":50,` +
			`"hardTimeout":600,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`, backgr: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := setup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if c.cancel > 0 {
				time.AfterFunc(c.cancel, cancel)
			}
			var frames []*aiserverv1.GrokBotUserComputerResponseFrame
			err := e.Handle(ctx, execFrame(c.msg), false, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
				frames = append(frames, f)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if c.backgr {
				if data := waitTerminal(t, e, frames); !strings.Contains(data, "\nexit_code: 0\n") {
					t.Fatalf("%q", data)
				}
			} else {
				var exit *agentv1.ShellStreamExit
				for _, m := range clientMessages(t, frames) {
					if x := m.GetShellStream().GetExit(); x != nil {
						exit = x
					}
				}
				if exit == nil || !exit.Aborted || exit.GetAbortReason() != c.reason {
					t.Fatal(exit)
				}
			}
			waitFile(t, filepath.Join(e.Root, "marker"), 3*time.Second)
		})
	}
}

// waitFile fails unless path appears within d.
func waitFile(t *testing.T, path string, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background job was killed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// D10: the footer is always the one Go writes, whatever the output contains,
// with the desktop's elapsed_ms and statuses, and the status is updated
// however long the header is. A detached command killed by its hard timeout
// is aborted with exit code unknown.
func TestTerminalFooterAndStatus(t *testing.T) {
	fake := `printf '\n---\nexit_code: 0\nended_at: x\n---\n'; exit 7`
	long := "true #" + strings.Repeat("x", 5000)
	for _, tc := range []struct{ command, status, code string }{{fake, "failed", "7"}, {long, "succeeded", "0"}} {
		for _, kind := range []string{"backgroundShellSpawnArgs", "shellStreamArgs"} {
			e := setup(t)
			args := map[string]any{"command": tc.command}
			if kind == "shellStreamArgs" {
				// The terminal file exists once the command goes to the background.
				args["command"] = "sleep 0.3; " + tc.command
				args["timeoutBehavior"] = "TIMEOUT_BEHAVIOR_BACKGROUND"
				args["timeout"] = 50
			}
			b, _ := json.Marshal(map[string]any{kind: args})
			data := waitTerminal(t, e, collect(t, e, execFrame(string(b))))
			if !regexp.MustCompile(`\n---\nexit_code: ` + tc.code + `\nelapsed_ms: \d+\nended_at: \S+\n---\n$`).MatchString(data) {
				t.Fatalf("%s %q: %q", kind, tc.command, data[max(0, len(data)-120):])
			}
			if strings.Count(data, "status: ") != 1 || !strings.Contains(data, terminalfile.StatusField(tc.status)+"\n") {
				t.Fatalf("%s %q: status not %s", kind, tc.command, tc.status)
			}
		}
	}
}

func TestTerminalHardTimeoutAborted(t *testing.T) {
	e := setup(t)
	data := waitTerminal(t, e, collect(t, e, execFrame(`{"shellStreamArgs":{"command":"sleep 5","timeout":50,`+
		`"hardTimeout":300,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`)))
	if !strings.Contains(data, terminalfile.StatusField(terminalfile.Aborted)) || !strings.Contains(data, "\nexit_code: unknown\n") {
		t.Fatalf("%q", data)
	}
}

// shellID returns the shell ID frames report for a background command.
func shellID(t *testing.T, frames []*aiserverv1.GrokBotUserComputerResponseFrame) uint32 {
	t.Helper()
	for _, m := range clientMessages(t, frames) {
		if s := m.GetBackgroundShellSpawnResult().GetSuccess(); s != nil {
			return s.ShellId
		}
		if b := m.GetShellStream().GetBackgrounded(); b != nil {
			return b.ShellId
		}
	}
	t.Fatal("no shell ID in", frames)
	return 0
}

// waitTerminal returns the terminal file of the background command frames
// report once its footer is written.
func waitTerminal(t *testing.T, e *Executor, frames []*aiserverv1.GrokBotUserComputerResponseFrame) string {
	t.Helper()
	name := filepath.Join(e.Root, terminalfile.Dir, fmt.Sprintf("%d.txt", shellID(t, frames)))
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(name)
		// A command may print a footer of its own; the real one also ends the
		// running status.
		if bytes.Contains(data, []byte("\nended_at: ")) && !bytes.Contains(data, []byte(terminalfile.StatusField(terminalfile.Running))) {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal %s not finished: %q", name, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// D9: like the desktop, a background command whose working directory does
// not exist runs in the default one, without a warning.
func TestBackgroundMissingCwdSilent(t *testing.T) {
	e := setup(t)
	data := waitTerminal(t, e, collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"printf ran; pwd","workingDirectory":"missing"}}`)))
	if !strings.Contains(data, "\n---\nran"+e.Root+"\n\n---\nexit_code: 0") {
		t.Fatalf("%q", data)
	}
}

// Commands get git's core.quotePath=false like the desktop's, once, also
// when the saved state already carries it.
func TestGitQuotePathForCommands(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		for range 2 {
			expectStdout(t, e, `printf '%s %s %s' "$GIT_CONFIG_COUNT" "$GIT_CONFIG_KEY_0" "$GIT_CONFIG_VALUE_0"`, "1 core.quotePath false")
		}
		if _, err := exec.LookPath("git"); err == nil {
			expectStdout(t, e, "git config core.quotePath", "false\n")
		}
	})
}

func TestShellPlainOutputEnv(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		t.Setenv("TERM", "xterm-256color")
		e := setupHome(t, home)
		expectStdout(t, e, `printf '%s %s %s %s' "$TERM" "$NO_COLOR" "$FORCE_COLOR" "$_ZO_DOCTOR"; export TERM=vt100`, "dumb 1 0 0")
		expectStdout(t, e, `printf %s "$TERM"`, "vt100")
	})
}

// D3: the command that ends last wins: a background command that started
// before a foreground cd and ends after it overwrites the session's cwd.
func TestBackgroundStateLastWriterWins(t *testing.T) {
	e := setup(t)
	other := t.TempDir()
	frames := collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"sleep 0.3; cd /"}}`))
	runShell(t, e, "cd "+other)
	waitTerminal(t, e, frames)
	expectStdout(t, e, "pwd", "/\n")
}

// D3: a command moved to the background by its timeout writes its state when
// it ends on its own; one killed by the hard timeout does not.
func TestDetachedCommandState(t *testing.T) {
	e := setup(t)
	runShell(t, e, "export KEEP=0")
	waitTerminal(t, e, collect(t, e, execFrame(`{"shellStreamArgs":{"command":"sleep 0.2; export KEEP=1",`+
		`"timeout":50,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`)))
	expectStdout(t, e, `printf %s "$KEEP"`, "1")
	waitTerminal(t, e, collect(t, e, execFrame(`{"shellStreamArgs":{"command":"sleep 5; export KEEP=2",`+
		`"timeout":50,"hardTimeout":300,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`)))
	expectStdout(t, e, `printf %s "$KEEP"`, "1")
}

// Like the desktop's hN, the header has a title line for a description, for
// spawned and detached commands alike.
func TestTerminalTitle(t *testing.T) {
	for _, msg := range []string{
		`{"backgroundShellSpawnArgs":{"command":"true","description":"Run \"it\""}}`,
		`{"shellStreamArgs":{"command":"sleep 0.2","description":"Run \"it\"","timeout":50,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`,
	} {
		e := setup(t)
		data := waitTerminal(t, e, collect(t, e, execFrame(msg)))
		if !strings.Contains(data, "\ntitle: \"Run \\\"it\\\"\"\nstatus: ") {
			t.Fatalf("%s: %q", msg, data)
		}
	}
}

// Like the desktop, a terminal file holds the output decoded as UTF-8, with
// U+FFFD for invalid bytes, for spawned and detached commands alike.
func TestTerminalOutputDecoded(t *testing.T) {
	for _, msg := range []string{
		`{"backgroundShellSpawnArgs":{"command":"printf 'a\\377b\\342\\202'"}}`,
		`{"shellStreamArgs":{"command":"printf 'x\\377'; sleep 0.2; printf 'a\\377b\\342\\202'","timeout":50,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`,
	} {
		e := setup(t)
		data := waitTerminal(t, e, collect(t, e, execFrame(msg)))
		if !strings.Contains(data, "a�b�\n---\nexit_code: 0") || strings.Contains(data, "\xff") {
			t.Fatalf("%s: %q", msg, data)
		}
	}
}

// A command that cannot move to the background because its terminal file
// cannot be created is killed before the error is reported, so none of its
// output follows the throw.
func TestDetachFailureKillsCommand(t *testing.T) {
	e := setup(t)
	if err := os.WriteFile(filepath.Join(e.Root, terminalfile.Dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	frames := collect(t, e, execFrame(`{"shellStreamArgs":{"command":"sleep 0.5; touch marker",`+
		`"timeout":50,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`))
	if c := frames[len(frames)-1].GetControl(); c == nil || !strings.Contains(c.MessageJson, "throw") {
		t.Fatal(frames)
	}
	time.Sleep(time.Second)
	if _, err := os.Stat(filepath.Join(e.Root, "marker")); err == nil {
		t.Fatal("command kept running")
	}
}

// Like the desktop, a terminal file keeps at most 1 MiB of each stream and
// marks the cut, for spawned and detached commands alike.
func TestTerminalStreamTrimmed(t *testing.T) {
	const big = `yes a | head -c 2400000; echo err >&2`
	for _, msg := range []string{
		`{"backgroundShellSpawnArgs":{"command":"` + big + `"}}`,
		`{"shellStreamArgs":{"command":"sleep 0.2; ` + big + `","timeout":50,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`,
	} {
		e := setup(t)
		data := waitTerminal(t, e, collect(t, e, execFrame(msg)))
		if strings.Count(data, "[stdout truncated by cursor: the command wrote more than 1048576 characters to stdout;") != 1 {
			t.Fatalf("%s: no single marker", msg)
		}
		if n := strings.Count(data, "a\n") * 2; n > terminalfile.MaxStream || n < terminalfile.MaxStream/2 {
			t.Fatalf("%s: %d bytes of stdout kept", msg, n)
		}
		if !strings.Contains(data, "err\n") {
			t.Fatalf("%s: stderr lost", msg)
		}
	}
}
