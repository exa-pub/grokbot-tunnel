package localexec

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
)

func TestBackgroundReplaysForegroundState(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		runShell(t, e, "grok_bg_fn() { printf bg-fn-ok; }; export BG_VAR=1")
		collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"grok_bg_fn; export BG_VAR=2"}}`))
		path := filepath.Join(e.Root, "terminals", "1.txt")
		deadline := time.Now().Add(2 * time.Second)
		var data []byte
		for !strings.Contains(string(data), "exit_code:") {
			if time.Now().After(deadline) {
				t.Fatalf("background did not finish: %q", data)
			}
			time.Sleep(10 * time.Millisecond)
			data, _ = os.ReadFile(path)
		}
		if !strings.Contains(string(data), "bg-fn-ok") || strings.Count(string(data), "exit_code:") != 1 {
			t.Fatalf("%q", data)
		}
		// Background commands do not change the foreground state. ≠: on the
		// desktop a background command that ends without abort writes its
		// state back to the shared session (rnd-v2/q4-background-state.md).
		expectStdout(t, e, `printf %s "$BG_VAR"`, "1")
	})
}

// Background jobs of a foreground command outlive it: its pipes are drained,
// not closed. Output that arrives shortly after the command exits is still
// forwarded, before the exit.
func TestShellBackgroundJobSurvivesCommand(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, "(sleep 0.2; echo late) &", "late\n")
		start := time.Now()
		runShell(t, e, "(sleep 1.5; echo later; echo later >&2; echo alive > marker) &")
		if time.Since(start) > 1400*time.Millisecond {
			t.Fatal("command waited for its background job")
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
	})
}

// The footer is always the one Go writes, whatever the output contains, and
// the status is updated however long the header is.
func TestTerminalFooterAndStatus(t *testing.T) {
	fake := `printf '\n---\nexit_code: 0\nended_at: x\n---\n'; exit 7`
	long := "true #" + strings.Repeat("x", 5000)
	for _, tc := range []struct{ command, status, code string }{{fake, "failed", "7"}, {long, "completed", "0"}} {
		for _, kind := range []string{"backgroundShellSpawnArgs", "shellStreamArgs"} {
			e := setup(t)
			args := map[string]any{"command": tc.command}
			if kind == "shellStreamArgs" {
				args["timeoutBehavior"] = "TIMEOUT_BEHAVIOR_BACKGROUND"
				args["timeout"] = 5000
			}
			b, _ := json.Marshal(map[string]any{kind: args})
			collect(t, e, execFrame(string(b)))
			data := waitTerminal(t, e, "1.txt")
			if !strings.Contains(data[len(data)-60:], "\n---\nexit_code: "+tc.code+"\nended_at: ") {
				t.Fatalf("%s %q: %q", kind, tc.command, data[max(0, len(data)-120):])
			}
			if strings.Count(data, "status: ") != 1 || !strings.Contains(data, terminalfile.StatusField(tc.status)+"\n") {
				t.Fatalf("%s %q: status not %s", kind, tc.command, tc.status)
			}
		}
	}
}

// waitTerminal returns terminal file name once its footer is written.
func waitTerminal(t *testing.T, e *Executor, name string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _ := os.ReadFile(filepath.Join(e.Root, "terminals", name))
		if terminalfile.Ended(data) {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal %s not finished: %q", name, data)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestBackgroundReportsMissingCwd(t *testing.T) {
	e := setup(t)
	collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"printf ran","workingDirectory":"missing"}}`))
	data := waitTerminal(t, e, "1.txt")
	if !strings.Contains(data, "---\nworking directory missing does not exist") || !strings.Contains(data, "instead\nran") {
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
