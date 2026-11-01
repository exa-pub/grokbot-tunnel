package localexec

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
)

// eachShell runs f with $SHELL set to bash and, if available, zsh
// ($GROK_TEST_ZSH or zsh on PATH). home is an empty directory for setupHome
// that profile files can be added to first.
func eachShell(t *testing.T, f func(t *testing.T, home string)) {
	shells := map[string]string{"bash": "/bin/bash"}
	if zsh := os.Getenv("GROK_TEST_ZSH"); zsh != "" {
		shells["zsh"] = zsh
	} else if zsh, err := exec.LookPath("zsh"); err == nil {
		shells["zsh"] = zsh
	}
	for name, path := range shells {
		t.Run(name, func(t *testing.T) {
			t.Setenv("SHELL", path)
			f(t, t.TempDir())
		})
	}
}

type shellResult struct {
	stdout, stderr string
	exit           *agentv1.ShellStreamExit
}

func runShell(t *testing.T, e *Executor, command string) shellResult {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"shellStreamArgs": map[string]any{"command": command}})
	return runShellFrame(t, e, string(b))
}

// runShellFrame runs the shell exec message msg.
func runShellFrame(t *testing.T, e *Executor, msg string) shellResult {
	t.Helper()
	var r shellResult
	for _, m := range clientMessages(t, collect(t, e, execFrame(msg))) {
		s := m.GetShellStream()
		r.stdout += s.GetStdout().GetData()
		r.stderr += s.GetStderr().GetData()
		if s.GetExit() != nil {
			r.exit = s.GetExit()
		}
	}
	if r.exit == nil {
		t.Fatalf("%s: no exit event", msg)
	}
	return r
}

func expectStdout(t *testing.T, e *Executor, command, want string) shellResult {
	t.Helper()
	r := runShell(t, e, command)
	if r.stdout != want {
		t.Fatalf("%q: stdout %q, want %q (stderr %q)", command, r.stdout, want, r.stderr)
	}
	return r
}

// A command must not be able to redirect the state dump into its output.
func TestShellStateDoesNotLeakIntoOutput(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		r := expectStdout(t, e, "export SECRET_TOKEN=abc; exec 3>&1 4>&1 5>&1; echo hi", "hi\n")
		if r.exit.Code != 0 {
			t.Fatal(r.exit)
		}
		expectStdout(t, e, `printf %s "$SECRET_TOKEN"`, "abc")
		for _, command := range []string{
			"exec 4>&1; f() { exit 3; }; f",
			"exec 4>&1; kill -TERM $$",
			"exec 4>&1; exec true",
			// The fallback snapshot's file name cannot be redirected.
			"__grok_fallback=/dev/stdout; exit 0",
		} {
			r := expectStdout(t, e, command, "")
			if strings.Contains(r.stderr, "SECRET_TOKEN") {
				t.Fatalf("%q: stderr %q", command, r.stderr)
			}
		}
		expectStdout(t, e, `printf %s "$SECRET_TOKEN"`, "abc")
	})
}

func TestShellStateSurvivesCommandTrapsAndDescriptors(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, "trap 'echo mine' EXIT; export A=1", "mine\n")
		expectStdout(t, e, "exec 4>&-; export B=2", "")
		expectStdout(t, e, "exec 4>/dev/null; export C=3", "")
		expectStdout(t, e, `printf %s "$A$B$C"`, "123")
	})
}

func TestShellChildrenInheritNoStateDescriptors(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("needs /proc")
	}
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, `(for fd in 3 4 5; do [ -e /proc/self/fd/$fd ] && echo "open $fd"; done; true) & wait`, "")
	})
}

func TestShellStateDropsVolatileSettings(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		runShell(t, e, "set -euo pipefail")
		expectStdout(t, e, `false | true; printf %s $?; false; printf after; printf %s "${UNSET_VAR_X}"`, "0after")
		first := runShell(t, e, "printf %s $SHLVL").stdout
		expectStdout(t, e, "printf %s $SHLVL", first)
	})
}

// The profile is read once, by an interactive login shell, so functions from
// the rc file are available and profile edits to PATH, EDITOR and SHLVL do not
// accumulate or override the agent's. Aliases are not expanded: an
// interactive alias such as rm -i would silently keep files.
func TestShellProfileReadOnce(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		profile := `export PATH="/grok-profile:$PATH"; export EDITOR=vim; echo run >> "$HOME/count"` + "\n"
		rc := "alias grok_alias='printf alias-ok'\nalias rm='rm -i'\ngrok_rc_fn() { printf fn-ok; }\n"
		files := map[string]string{".bash_profile": profile + ". \"$HOME/.bashrc\"\n", ".bashrc": rc, ".zprofile": profile, ".zshrc": rc}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(home, name), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		e := setupHome(t, home)
		runShell(t, e, "export EDITOR=nano")
		level := runShell(t, e, "printf %s $SHLVL").stdout
		for range 3 {
			expectStdout(t, e, `printf %s "$PATH" | tr : '\n' | grep -c grok-profile; printf '%s|%s' "$EDITOR" "$SHLVL"`, "1\nnano|"+level)
		}
		expectStdout(t, e, "grok_rc_fn; grok_alias 2>/dev/null || printf '|no-alias'", "fn-ok|no-alias")
		expectStdout(t, e, "echo x > victim; rm victim </dev/null; [ -e victim ] || printf removed", "removed")
		if b, _ := os.ReadFile(filepath.Join(home, "count")); string(b) != "run\n" {
			t.Fatalf("profile ran %q", b)
		}
	})
}

// startupFile makes bash ($BASH_ENV) and zsh (.zshenv) run script when they
// start.
func startupFile(t *testing.T, home, script string) {
	t.Helper()
	startup := filepath.Join(home, "startup.sh")
	if err := os.WriteFile(startup, []byte(script+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASH_ENV", startup)
	if err := os.WriteFile(filepath.Join(home, ".zshenv"), []byte(". '"+startup+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// S2-5, like the desktop's B3 and U3: a startup file that closes or replaces
// descriptor 5 makes the command fail with the desktop's text and exit code
// 126; from then on the session passes commands as arguments, so the next
// foreground and background commands run, with the state kept.
func TestShellStartupFileClosingCommandDescriptor(t *testing.T) {
	const text = "The command did not run: a shell startup file ($BASH_ENV or .zshenv) closed or replaced file " +
		"descriptor 5, which carries the command. Run it again; this shell now passes commands as arguments.\n"
	for name, script := range map[string]string{"closed": "exec 5<&-", "empty": "exec 5</dev/null", "write-only": `exec 5>"$HOME/startup.log"`} {
		t.Run(name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, home string) {
				startupFile(t, home, script)
				e := setupHome(t, home)
				r := runShell(t, e, "export KEPT=1; printf ran")
				if r.stdout != "" || r.exit.Code != 126 || !strings.HasSuffix(r.stderr, text) {
					t.Fatalf("%q %q %v", r.stdout, r.stderr, r.exit)
				}
				expectStdout(t, e, `export KEPT=2; printf 'ran|%s' "${KEPT}"`, "ran|2")
				expectStdout(t, e, `printf 'ran|%s' "${KEPT}"`, "ran|2")
				frames := collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"printf 'bg|%s' \"$KEPT\""}}`))
				if data := waitTerminal(t, e, frames); !strings.Contains(data, "---\nbg|2\n---\n") {
					t.Fatalf("%q", data)
				}
			})
		})
	}
}

// Like the desktop, which skips the replay when it cannot read descriptor 3,
// a startup file that closes it does not stop commands from running.
func TestShellStartupFileClosingReplayDescriptor(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		startupFile(t, home, "exec 3<&-")
		e := setupHome(t, home)
		expectStdout(t, e, "export KEPT=1; printf ran", "ran")
		expectStdout(t, e, `printf 'ran|%s' "${KEPT}"`, "ran|1")
	})
}

// D4: leaving Root with cd is kept, like on the desktop; the next command
// starts there with the rest of the state.
func TestShellCwdOutsideRootKeepsState(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		r := runShell(t, e, "export KEPT=1; cd /")
		if r.exit.Cwd != "/" || r.stderr != "" {
			t.Fatal(r.exit, r.stderr)
		}
		expectStdout(t, e, `printf '%s|' "$KEPT"; pwd`, "1|/\n")
	})
}

// Like the desktop, a working directory removed by the command is kept. The
// next command without a working directory does not run and exits with 1;
// one with a working directory runs there.
func TestShellRemovedCwdKept(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		gone := filepath.Join(e.Root, "gone")
		r := runShell(t, e, "mkdir gone && cd gone && rmdir ../gone")
		if r.exit.Cwd != gone || r.stderr != "" {
			t.Fatal(r.exit, r.stderr)
		}
		for range 2 {
			r = runShell(t, e, "echo ran")
			want := "The shell's working directory no longer exists: " + gone +
				". The command did not run. Re-run it with working_directory set to an existing directory.\n"
			if r.stdout != "" || r.stderr != want || r.exit.Code != 1 || r.exit.Cwd != gone || r.exit.Aborted || r.exit.LocalExecutionTimeMs == nil {
				t.Fatal(r.exit, r.stdout, r.stderr)
			}
		}
		data := waitTerminal(t, e, collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"echo ran"}}`)))
		if !strings.Contains(data, "\n---\nThe shell's working directory no longer exists: "+gone+". ") ||
			!strings.Contains(data, "existing directory.\n\n---\nexit_code: 1\n") || strings.Contains(data, "pid:") ||
			!strings.Contains(data, terminalfile.StatusField(terminalfile.Failed)) {
			t.Fatalf("%q", data)
		}
		b, _ := json.Marshal(map[string]any{"shellStreamArgs": map[string]any{"command": "pwd", "workingDirectory": e.Root}})
		r = runShellFrame(t, e, string(b))
		if r.stdout != e.Root+"\n" || r.exit.Cwd != e.Root {
			t.Fatal(r.exit, r.stdout, r.stderr)
		}
		expectStdout(t, e, "pwd", e.Root+"\n")
	})
}

// When /usr/bin/env fails in the state dump, the next command keeps the
// environment this one started with instead of starting with an empty one
// (bug 2 of the 2026-10-31 review), silently, as the desktop keeps its state.
// A variable above the kernel's 128 KiB limit for a single string makes
// execve fail with E2BIG.
func TestShellKeepsEnvWhenDumpFails(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, "export GROK_KEEP=1", "")
		r := runShell(t, e, `export GROK_BIG="$(printf '%0200000d' 0)"; export GROK_KEEP=2; cd /`)
		if r.stderr != "" || r.exit.Cwd != "/" {
			t.Fatal(r.exit, r.stderr)
		}
		expectStdout(t, e, `printf '%s|%s|' "$GROK_KEEP" "${PATH:+path}"; pwd`, "1|path|/\n")
	})
}

// Like the desktop, a state above the old 1 MiB limit is kept.
func TestShellLargeStateKept(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		runShell(t, e, `for i in 1 2 3 4 5 6 7 8 9 10 11 12; do export GROK_LARGE_$i="$(printf '%0100000d' $i)"; done; export GROK_SMALL=1`)
		expectStdout(t, e, `printf '%s|%s' "$GROK_SMALL" "${#GROK_LARGE_12}"`, "1|100000")
	})
}

func TestShellOptionsAndFunctionsPersist(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, "set -o noclobber; alias grok_a='printf a'; grok_f() { printf f; }", "")
		r := expectStdout(t, e, "[[ -o noclobber ]] && grok_f; grok_a 2>/dev/null || printf '|no-alias'", "f|no-alias")
		if r.stderr != "" {
			t.Fatal(r.stderr)
		}
	})
}

// The command text is not left in the positional parameters.
func TestShellCommandHasNoPositionalParameters(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, `echo "$#|${1:-none}"; for arg; do echo "arg $arg"; done`, "0|none\n")
		expectStdout(t, e, "set -u; echo \"${1-unset}\"", "unset\n")
	})
	t.Setenv("SHELL", "/bin/bash")
	e := setup(t)
	// eval reports the command's own line numbers.
	expectStdout(t, e, "echo $LINENO\necho $LINENO", "1\n2\n")
}

// xtrace and verbose would trace the replay of every later command.
func TestShellTracingNotCarriedOver(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		runShell(t, e, "grok_f() { :; }; set -xv")
		r := expectStdout(t, e, "echo a", "a\n")
		if r.stderr != "" {
			t.Fatalf("stderr %q", r.stderr)
		}
	})
}

// A command that ends the shell itself, as with set -e, still keeps its
// state through the EXIT trap. Without any snapshot the previous state stays.
func TestShellStateSavedWhenCommandExits(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		sub := filepath.Join(e.Root, "sub")
		r := runShell(t, e, "mkdir sub; cd sub; export Z=1; set -e; false; echo unreachable")
		if r.stdout != "" || r.stderr != "" || r.exit.Code != 1 || r.exit.Cwd != sub {
			t.Fatal(r)
		}
		r = runShell(t, e, "export Y=2; f() { exit 4; }; f")
		if r.exit.Code != 4 || r.stderr != "" {
			t.Fatal(r)
		}
		expectStdout(t, e, `printf '%s%s|' "$Z" "$Y"; pwd`, "12|"+sub+"\n")
		// Like the desktop, a command that left no state changes nothing,
		// silently.
		r = runShell(t, e, "export X=3; cd ..; trap - EXIT; exit 5")
		if r.exit.Code != 5 || r.exit.Cwd != sub || r.stderr != "" {
			t.Fatal(r)
		}
		expectStdout(t, e, `printf '%s|' "$X"; pwd`, "|"+sub+"\n")
		// Subshells do not save their state.
		expectStdout(t, e, `(cd /; export W=1; exit 0); printf '%s|' "$W"; exit 0`, "|")
		expectStdout(t, e, `printf '%s|' "$W"; pwd`, "|"+sub+"\n")
	})
}

// Reading the profile does not hold up a canceled command; the profile still
// applies to the next one.
func TestShellInitHonorsCancellation(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		profile := "sleep 1; export FROM_PROFILE=1\n"
		for _, name := range []string{".bash_profile", ".zprofile"} {
			if err := os.WriteFile(filepath.Join(home, name), []byte(profile), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		e := setupHome(t, home)
		for _, command := range []string{
			`{"shellStreamArgs":{"command":"true"}}`,
			`{"backgroundShellSpawnArgs":{"command":"true"}}`,
		} {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			start := time.Now()
			err := e.Handle(ctx, execFrame(command), false, func(*aiserverv1.GrokBotUserComputerResponseFrame) error { return nil })
			cancel()
			if err != nil || time.Since(start) > 500*time.Millisecond {
				t.Fatal(err, time.Since(start))
			}
		}
		expectStdout(t, e, `printf %s "$FROM_PROFILE"`, "1")
	})
}

// noclobber is kept between commands; the EXIT trap still overwrites its
// fallback file.
func TestShellStateSavedUnderNoclobber(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		runShell(t, e, "set -o noclobber")
		r := runShell(t, e, "export NC=1; exit 3")
		if r.exit.Code != 3 || r.stderr != "" {
			t.Fatal(r.exit, r.stderr)
		}
		expectStdout(t, e, `printf %s "$NC"; [[ -o noclobber ]] && printf '|on'`, "1|on")
	})
}

// "set -a", which is kept between commands, does not carry the scripts'
// variables over, which would disable the EXIT trap.
func TestShellAllexportKeepsScriptVariablesOut(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		runShell(t, e, "set -a")
		expectStdout(t, e, `env | grep -c '^__grok_'; printf '%s|' "${__grok_dumped-unset}"; export AE=1; exit 3`, "0\nunset|")
		expectStdout(t, e, `printf %s "$AE"; [[ -o allexport ]] && printf '|on'`, "1|on")
	})
}

// Like the desktop's session, which is cloned into the root, the first
// command starts in the root even if the profile changes directory.
func TestShellProfileCdIgnored(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		for _, name := range []string{".bash_profile", ".zprofile"} {
			if err := os.WriteFile(filepath.Join(home, name), []byte("cd /; export FROM_PROFILE=1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		e := setupHome(t, home)
		expectStdout(t, e, `printf '%s|' "$FROM_PROFILE"; pwd`, "1|"+e.Root+"\n")
	})
}

// Like the desktop's dS, commands get AGENT_TRANSCRIPTS and the request's
// CURSOR_CONVERSATION_ID and CURSOR_REQUEST_ID; the latter are not carried
// over, nor are proxy settings, as in the desktop's dump.
func TestShellRequestEnvAndScopedVariables(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		msg := `{"shellStreamArgs":{"command":"printf '%s|%s|%s' \"$AGENT_TRANSCRIPTS\" \"$CURSOR_CONVERSATION_ID\" \"$CURSOR_REQUEST_ID\"; ` +
			`export http_proxy=x HTTPS_PROXY=y KEEP=1","conversationId":"c 1","requestId":"r-1"}}`
		r := runShellFrame(t, e, msg)
		if want := filepath.Join(e.Root, "agent-transcripts") + "|c_201|r-1"; r.stdout != want {
			t.Fatalf("%q, want %q", r.stdout, want)
		}
		expectStdout(t, e, `printf '%s|%s|%s|%s|%s' "$KEEP" "$http_proxy" "$HTTPS_PROXY" "$CURSOR_REQUEST_ID" "${AGENT_TRANSCRIPTS:+set}"`, "1||||set")
		frames := collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"printf '%s|%s' \"$CURSOR_REQUEST_ID\" \"$CURSOR_CONVERSATION_ID\"",`+
			`"requestId":"bg","conversationId":"conv"}}`))
		if data := waitTerminal(t, e, frames); !strings.Contains(data, "---\nbg|conv\n---\n") {
			t.Fatalf("%q", data)
		}
	})
}

// Like the desktop, the exit reports the session's cwd: a command that ran
// elsewhere and left no state does not change it.
func TestShellExitCwdWithoutState(t *testing.T) {
	e := setup(t)
	b, _ := json.Marshal(map[string]any{"shellStreamArgs": map[string]any{"command": "exec true", "workingDirectory": t.TempDir()}})
	if r := runShellFrame(t, e, string(b)); r.exit.Cwd != e.Root {
		t.Fatal(r.exit)
	}
}

// Like the desktop's ilt, a working directory of only whitespace is the
// session's cwd.
func TestShellBlankWorkingDirectory(t *testing.T) {
	e := setup(t)
	runShell(t, e, "mkdir sub && cd sub")
	r := runShellFrame(t, e, `{"shellStreamArgs":{"command":"pwd","workingDirectory":"  "}}`)
	if r.stdout != filepath.Join(e.Root, "sub")+"\n" || r.stderr != "" {
		t.Fatal(r.stdout, r.stderr)
	}
}

// shellTemp isolates the temporary files of executors in a new $TMPDIR,
// which it returns.
func shellTemp(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	return tmp
}

// tempEntries returns the names in dir.
func tempEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// S2-3: the fallback snapshots of shells that outlive the executor do not
// stay in $TMPDIR: they live in a private directory that Close removes, so
// the EXIT trap of a shell that exits later writes nothing.
func TestShellStateFilesRemovedOnClose(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		tmp := shellTemp(t)
		e := setupHome(t, home)
		runShell(t, e, "export GROK_SECRET=hunter2")
		collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"sleep 0.5; exit 0"}}`))
		collect(t, e, execFrame(`{"shellStreamArgs":{"command":"sleep 0.5; exit 3","timeout":10,"timeoutBehavior":"TIMEOUT_BEHAVIOR_BACKGROUND"}}`))
		names := tempEntries(t, tmp)
		if len(names) != 1 || !strings.HasPrefix(names[0], "grok-shell-") {
			t.Fatal(names)
		}
		if info, err := os.Stat(filepath.Join(tmp, names[0])); err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
			t.Fatal(info, err)
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if names := tempEntries(t, tmp); len(names) != 0 {
				t.Fatal(names)
			}
			time.Sleep(time.Second) // the shells exit
		}
	})
}

// S2-3, S3-1: a new executor removes the shell directories that no executor
// holds locked in $TMPDIR, also those named after a running process, as one
// in another pid namespace that shares /tmp would be, and the fallback
// snapshots of older versions that no process has open; it keeps the
// directories that executors hold locked.
func TestShellStaleStateFilesRemoved(t *testing.T) {
	tmp := shellTemp(t)
	stale := []string{
		"grok-shell-state-123",
		"grok-shell-" + strconv.Itoa(os.Getpid()) + "-1", // left by an executor of this process
		"grok-shell-1-1",                                 // named after init, which runs
	}
	kept := []string{
		"grok-shell-state-456",   // an older version's, open while its command runs
		"grok-shell-999999999-1", // locked by an executor in another pid namespace
		"grok-shell-other",
	}
	for _, name := range []string{stale[0], kept[0]} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte("GROK_SECRET=hunter2"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	open, err := os.Open(filepath.Join(tmp, kept[0]))
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	for _, dir := range append(stale[1:], kept[1:]...) {
		if err := os.Mkdir(filepath.Join(tmp, dir), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, dir, "fallback"), []byte("GROK_SECRET=hunter2"), 0o600); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Hour) // a fresh one is kept for its session to lock
		if err := os.Chtimes(filepath.Join(tmp, dir), old, old); err != nil {
			t.Fatal(err)
		}
	}
	held := flockShared(t, filepath.Join(tmp, kept[1]))
	defer held.Close()
	other := setup(t) // another executor of this process, which holds its own
	names := tempEntries(t, tmp)
	for _, name := range stale {
		if slices.Contains(names, name) {
			t.Fatal("stale", name, "kept")
		}
	}
	for _, name := range kept {
		if !slices.Contains(names, name) {
			t.Fatal(name, "removed")
		}
	}
	kept = names
	e := setup(t)
	expectStdout(t, e, "printf ran", "ran")
	names = tempEntries(t, tmp)
	for _, name := range kept {
		if !slices.Contains(names, name) {
			t.Fatal(name, "removed")
		}
	}
	if len(names) != len(kept)+1 {
		t.Fatal(names)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if err := other.Close(); err != nil {
		t.Fatal(err)
	}
	if names := tempEntries(t, tmp); len(names) != len(kept)-1 {
		t.Fatal(names)
	}
}

// flockShared opens the directory at path and takes a shared flock on it,
// as an executor holds its private directory.
func flockShared(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		t.Fatal(err)
	}
	return f
}

// privateDir returns the private directory of the only executor in tmp.
func privateDir(t *testing.T, tmp string) string {
	t.Helper()
	names := tempEntries(t, tmp)
	if len(names) != 1 || !strings.HasPrefix(names[0], "grok-shell-") {
		t.Fatal(names)
	}
	return filepath.Join(tmp, names[0])
}

// S3-1: a private directory removed under a live executor, as
// systemd-tmpfiles does with an idle one, is created again by the next
// command, and foreground and background commands keep working with the
// state kept. Commands do not inherit the descriptor of its lock.
func TestShellPrivateDirRecreated(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		tmp := shellTemp(t)
		e := setupHome(t, home)
		expectStdout(t, e, "export KEPT=1; printf ran", "ran")
		old := privateDir(t, tmp)
		if err := os.RemoveAll(old); err != nil {
			t.Fatal(err)
		}
		expectStdout(t, e, `printf 'ran|%s' "$KEPT"`, "ran|1")
		dir := privateDir(t, tmp)
		if dir == old {
			t.Fatal("not recreated")
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		frames := collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"printf 'bg|%s' \"$KEPT\""}}`))
		if data := waitTerminal(t, e, frames); !strings.Contains(data, "---\nbg|1\n---\n") {
			t.Fatalf("%q", data)
		}
		if _, err := os.Stat("/proc/self/fd"); err == nil {
			r := runShell(t, e, `for f in /proc/self/fd/*; do readlink "$f"; done`)
			if strings.Contains(r.stdout, "grok-shell-") {
				t.Fatalf("%q", r.stdout)
			}
		}
		if err := e.Close(); err != nil {
			t.Fatal(err)
		}
		if names := tempEntries(t, tmp); len(names) != 0 {
			t.Fatal(names)
		}
	})
}

// S3-1: systemd-tmpfiles, which removes what is older than the age of /tmp
// but skips a locked directory (tmpfiles.d(5)), keeps the private directory
// of an idle executor.
func TestShellPrivateDirSurvivesTmpfiles(t *testing.T) {
	tmpfiles, err := exec.LookPath("systemd-tmpfiles")
	if err != nil {
		t.Skip("needs systemd-tmpfiles")
	}
	tmp := shellTemp(t)
	conf := filepath.Join(t.TempDir(), "tmp.conf")
	if err := os.WriteFile(conf, []byte("d "+tmp+" 1777 - - 1s\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := setup(t)
	expectStdout(t, e, "printf ran", "ran")
	dir := privateDir(t, tmp)
	stray := filepath.Join(tmp, "stray")
	if err := os.Mkdir(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2500 * time.Millisecond) // idle longer than the age
	if out, err := exec.CommandContext(t.Context(), tmpfiles, "--clean", conf).CombinedOutput(); err != nil {
		t.Skipf("systemd-tmpfiles: %v %s", err, out)
	}
	if _, err := os.Stat(stray); err == nil {
		t.Skip("systemd-tmpfiles did not clean " + tmp)
	}
	if got := privateDir(t, tmp); got != dir {
		t.Fatal(got, dir)
	}
	expectStdout(t, e, "printf ran", "ran")
}

// S3-2, like the desktop, whose shells start with the app's environment and
// replay the saved one inside: the variables a shell reads at startup that a
// command exported reach the next command and its children, not the shell
// that runs it. An exported SHELLOPTS does not carry nounset and pipefail
// into later shells.
func TestShellStartupVariablesNotUsedByLaterShells(t *testing.T) {
	unsetenv := func(t *testing.T, key string) {
		t.Helper()
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("SHELLOPTS", func(t *testing.T) {
		t.Setenv("SHELL", "/bin/bash")
		e := setup(t)
		expectStdout(t, e, "set -u -o pipefail; export SHELLOPTS BASHOPTS; echo ok", "ok\n")
		expectStdout(t, e, `false | true; echo "pipe=$? x=$UNDEFINED_VAR"; case :$SHELLOPTS: in *:pipefail:*|*:nounset:*) echo kept;; esac`, "pipe=0 x=\n")
	})
	t.Run("BASH_ENV", func(t *testing.T) {
		t.Setenv("SHELL", "/bin/bash")
		unsetenv(t, "BASH_ENV")
		home := t.TempDir()
		rc := filepath.Join(home, "it's rc.sh")
		if err := os.WriteFile(rc, []byte("echo sourced\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		e := setupHome(t, home)
		expectStdout(t, e, `export BASH_ENV="$HOME/it's rc.sh"; echo set`, "set\n")
		expectStdout(t, e, `printf '%s\n' "$BASH_ENV"; bash -c 'echo child'`, rc+"\nsourced\nchild\n")
		expectStdout(t, e, "unset BASH_ENV; echo unset", "unset\n")
		t.Setenv("BASH_ENV", rc) // the CLI's own runs in every shell
		expectStdout(t, e, `printf '%s\n' "${BASH_ENV-none}"`, "sourced\nnone\n")
	})
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Log("no zsh")
		return
	}
	fake := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, ".zshenv"), []byte("echo FAKE-ZSHENV\nexec 5<&-\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	t.Run("ZDOTDIR", func(t *testing.T) {
		t.Setenv("SHELL", zsh)
		e := setup(t)
		dir := fake(t)
		expectStdout(t, e, "export ZDOTDIR='"+dir+"'; echo set", "set\n")
		expectStdout(t, e, `printf '%s\n' "$ZDOTDIR"`, dir+"\n")
		expectStdout(t, e, `zsh -c 'echo child'`, "FAKE-ZSHENV\nchild\n")
	})
	t.Run("HOME", func(t *testing.T) {
		t.Setenv("SHELL", zsh)
		e := setup(t)
		unsetenv(t, "ZDOTDIR")
		dir := fake(t)
		expectStdout(t, e, "export HOME='"+dir+"'; echo set", "set\n")
		expectStdout(t, e, `printf '%s\n' "$HOME" ~`, dir+"\n"+dir+"\n")
	})
}

// S3-2: an exported IFS is restored, as the desktop's replay of "export -p"
// does; bash and zsh do not take it from the environment.
func TestShellExportedIFSRestored(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, "export IFS=:; echo set", "set\n")
		expectStdout(t, e, `printf '[%s]' "$IFS"; echo a:b | { read -r x y; printf '%s|%s' "$x" "$y"; }`, "[:]a|b")
	})
}
