package localexec

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
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
	var r shellResult
	for _, m := range clientMessages(t, collect(t, e, execFrame(string(b)))) {
		s := m.GetShellStream()
		r.stdout += s.GetStdout().GetData()
		r.stderr += s.GetStderr().GetData()
		if s.GetExit() != nil {
			r.exit = s.GetExit()
		}
	}
	if r.exit == nil {
		t.Fatalf("%q: no exit event", command)
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

func TestShellStartupFileClosingCommandDescriptor(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		startup := filepath.Join(home, "startup.sh")
		if err := os.WriteFile(startup, []byte("exec 5<&-\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BASH_ENV", startup)
		if err := os.WriteFile(filepath.Join(home, ".zshenv"), []byte(". "+startup+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		e := setupHome(t, home)
		r := runShell(t, e, "printf ran")
		if r.stdout != "" || r.exit.Code != 126 || !strings.Contains(r.stderr, "did not run") {
			t.Fatal(r)
		}
	})
}

// Leaving Root with cd keeps the rest of the state; the next command starts
// where the previous one did.
func TestShellCwdOutsideRootKeepsState(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		r := runShell(t, e, "export KEPT=1; cd /")
		if r.exit.Cwd != e.Root || !strings.Contains(r.stderr, "Working directory / is outside") {
			t.Fatal(r.exit)
		}
		expectStdout(t, e, `printf '%s|' "$KEPT"; pwd`, "1|"+e.Root+"\n")
	})
}

// When /usr/bin/env fails in the state dump, the next command keeps the
// previous environment instead of starting with an empty one (bug 2 of the
// 2026-10-31 review). A variable above the kernel's 128 KiB limit for a single
// string makes execve fail with E2BIG.
func TestShellKeepsEnvWhenDumpFails(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		expectStdout(t, e, "export GROK_KEEP=1", "")
		r := runShell(t, e, `export GROK_BIG="$(printf '%0200000d' 0)"; export GROK_KEEP=2`)
		if !strings.Contains(r.stderr, "environment could not be saved") {
			t.Fatalf("no notice: %q", r.stderr)
		}
		expectStdout(t, e, `printf '%s|%s' "$GROK_KEEP" "${PATH:+path}"`, "1|path")
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
// state through the EXIT trap. Without any snapshot the agent is told that the
// state was not kept.
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
		r = runShell(t, e, "export X=3; cd ..; trap - EXIT; exit 5")
		if r.exit.Code != 5 || r.exit.Cwd != sub || !strings.Contains(r.stderr, "were not kept") {
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
