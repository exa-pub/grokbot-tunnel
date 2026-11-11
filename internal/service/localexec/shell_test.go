package localexec

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	aiserverv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/aiserver/v1"
)

func TestShellTimeoutAndCancel(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "cancel"}[canceled], func(t *testing.T) {
			e := setup(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if canceled {
				time.AfterFunc(100*time.Millisecond, cancel)
			}
			var mu sync.Mutex
			var output strings.Builder
			start := time.Now()
			err := e.Handle(ctx, execFrame(`{"id":8,"shellStreamArgs":{"command":"printf hello; printf problem >&2; sleep 20","timeout":200}}`), false, func(f *aiserverv1.GrokBotUserComputerResponseFrame) error {
				mu.Lock()
				defer mu.Unlock()
				if f.GetClient() != nil {
					output.WriteString(f.GetClient().MessageJson)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if time.Since(start) > 3*time.Second {
				t.Fatal("did not kill process group promptly")
			}
			s := output.String()
			if !strings.Contains(s, "hello") || !strings.Contains(s, "problem") || !strings.Contains(s, `"aborted":true`) {
				t.Fatal(s)
			}
		})
	}
}

func TestBackgroundCommands(t *testing.T) {
	for _, kind := range []string{"backgroundShellSpawnArgs", "shellStreamArgs"} {
		t.Run(kind, func(t *testing.T) {
			e := setup(t)
			args := map[string]any{"command": "sleep 0.15; printf finished"}
			if kind == "shellStreamArgs" {
				args["timeout"] = 30
				args["timeoutBehavior"] = "TIMEOUT_BEHAVIOR_BACKGROUND"
			}
			b, _ := json.Marshal(map[string]any{"id": 10, kind: args})
			start := time.Now()
			frames := collect(t, e, execFrame(string(b)))
			if time.Since(start) > time.Second {
				t.Fatal("background spawn blocked")
			}
			if len(frames) == 0 || frames[len(frames)-2].GetClient() == nil {
				t.Fatal("missing background result")
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				entries, _ := os.ReadDir(filepath.Join(e.Root, "terminals"))
				found := false
				for _, entry := range entries {
					b, _ := os.ReadFile(filepath.Join(e.Root, "terminals", entry.Name()))
					found = found || strings.Contains(string(b), "finished")
				}
				if found {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("background output not captured")
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestShellExitCode(t *testing.T) {
	e := setup(t)
	frames := collect(t, e, execFrame(`{"id":5,"shellStreamArgs":{"command":"exit 3"}}`))
	last := frames[len(frames)-2].GetClient()
	if last == nil || !strings.Contains(last.MessageJson, `"code":3`) {
		t.Fatal(frames)
	}
}

func TestBackgroundDoesNotWaitForForeground(t *testing.T) {
	e := setup(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		collect(t, e, execFrame(`{"shellStreamArgs":{"command":"sleep 1"}}`))
	}()
	time.Sleep(100 * time.Millisecond) // let the foreground command start
	start := time.Now()
	collect(t, e, execFrame(`{"backgroundShellSpawnArgs":{"command":"true"}}`))
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("background spawn waited for the foreground command")
	}
	<-done
}

// Like the desktop, foreground commands run in parallel, each from the state
// of the moment it starts, and the one that ends last wins: a command started
// before a "cd" and ending after it puts the old cwd back. A command whose
// environment could not be saved keeps the one it started with, not one a
// parallel command left.
func TestShellParallelLastWriterWins(t *testing.T) {
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		runShell(t, e, "mkdir sub; export X=0")
		slow := make(chan shellResult)
		go func() {
			slow <- runShell(t, e, `sleep 1; export P=1 GROK_BIG="$(printf '%0200000d' 0)"`)
		}()
		time.Sleep(200 * time.Millisecond) // let the slow command start
		start := time.Now()
		r := runShell(t, e, "cd sub; export X=1 Q=1")
		if time.Since(start) > 700*time.Millisecond || r.exit.Cwd != filepath.Join(e.Root, "sub") {
			t.Fatal("waited for the running command:", time.Since(start), r.exit)
		}
		expectStdout(t, e, `printf %s "$X"; pwd`, "1"+filepath.Join(e.Root, "sub")+"\n")
		if r := <-slow; r.exit.Cwd != e.Root {
			t.Fatal(r.exit)
		}
		expectStdout(t, e, `printf '%s|%s|%s|' "$X" "$Q" "$P"; pwd`, "0|||"+e.Root+"\n")
	})
}

// Parallel commands do not wait for each other.
func TestShellParallelCommands(t *testing.T) {
	e := setup(t)
	start := time.Now()
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runShell(t, e, fmt.Sprintf("sleep 1; export P%d=1", i))
		}()
	}
	wg.Wait()
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatal("commands ran one at a time:", d)
	}
	r := runShell(t, e, `printf %s "$P0$P1$P2$P3"`)
	if r.stdout != "1" {
		t.Fatal(r.stdout) // the last one to end wins
	}
}

// Like the desktop's D3, with fileOutputThresholdBytes an output above the
// threshold also goes to <root>/agent-tools/<uuid>.txt, stdout and stderr as
// they arrive, which the exit reports; the agent still gets at most 1 MiB of
// each stream. A smaller output goes nowhere.
func TestShellFullOutputFile(t *testing.T) {
	e := setup(t)
	small := runShellFrame(t, e, `{"shellStreamArgs":{"command":"echo hi","fileOutputThresholdBytes":"100"}}`)
	if small.exit.OutputLocation != nil {
		t.Fatal(small.exit)
	}
	r := runShellFrame(t, e, `{"shellStreamArgs":{"command":"echo err >&2; sleep 0.2; yes a | head -c 1500000; printf 'end\\377'",`+
		`"fileOutputThresholdBytes":"1000"}}`)
	loc := r.exit.OutputLocation
	if loc == nil || filepath.Dir(loc.FilePath) != filepath.Join(e.Root, "agent-tools") || !strings.HasSuffix(loc.FilePath, ".txt") {
		t.Fatal(r.exit)
	}
	data, err := os.ReadFile(loc.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	want := "err\n" + strings.Repeat("a\n", 750000) + "end�"
	if string(data) != want || loc.SizeBytes != 1500008 || loc.LineCount != 750001 {
		t.Fatalf("%d bytes, location %v", len(data), loc)
	}
	if len(r.stdout) > 1<<20 || r.stderr != "err\n" {
		t.Fatal(len(r.stdout), r.stderr)
	}
}

// If the full output cannot be saved, the agent is told on stderr, as on the
// desktop, and the exit has no output location.
func TestShellFullOutputFileFails(t *testing.T) {
	e := setup(t)
	if err := os.WriteFile(filepath.Join(e.Root, "agent-tools"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := runShellFrame(t, e, `{"shellStreamArgs":{"command":"head -c 2000 /dev/zero","fileOutputThresholdBytes":"1000"}}`)
	prefix := "\nThe full output could not be saved to " + filepath.Join(e.Root, "agent-tools") + "/"
	if r.exit.OutputLocation != nil || !strings.HasPrefix(r.stderr, prefix) || !strings.HasSuffix(r.stderr, "\n") {
		t.Fatal(r.exit, r.stderr)
	}
}

// Linux limits a single exec argument to 128 KiB; state and commands must not
// be passed as arguments.
func TestShellStateAndCommandsAboveArgumentLimit(t *testing.T) {
	t.Setenv("SHELL", "/bin/bash")
	e := setup(t)
	body := strings.Repeat(": padding; ", 16<<10) // about 176 KiB once saved
	define, _ := json.Marshal(map[string]any{"shellStreamArgs": map[string]any{"command": "big_fn() { " + body + "printf fn-ok; }"}})
	collect(t, e, execFrame(string(define)))
	stdout := func(frames []*aiserverv1.GrokBotUserComputerResponseFrame) string {
		var out strings.Builder
		for _, m := range clientMessages(t, frames) {
			out.WriteString(m.GetShellStream().GetStdout().GetData())
		}
		return out.String()
	}
	if got := stdout(collect(t, e, execFrame(`{"shellStreamArgs":{"command":"big_fn"}}`))); got != "fn-ok" {
		t.Fatalf("large function not replayed: %q", got)
	}
	large := strings.Repeat("x", 200<<10)
	command, _ := json.Marshal(map[string]any{"shellStreamArgs": map[string]any{"command": "v=" + large + "; printf %d ${#v}"}})
	if got := stdout(collect(t, e, execFrame(string(command)))); got != strconv.Itoa(len(large)) {
		t.Fatalf("large command failed: %q", got)
	}
	background, _ := json.Marshal(map[string]any{"backgroundShellSpawnArgs": map[string]any{"command": "v=" + large + "; printf bg-%d ${#v}"}})
	collect(t, e, execFrame(string(background)))
	want := "bg-" + strconv.Itoa(len(large))
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, _ := os.ReadDir(filepath.Join(e.Root, "terminals"))
		for _, entry := range entries {
			if b, _ := os.ReadFile(filepath.Join(e.Root, "terminals", entry.Name())); strings.Contains(string(b), want) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("large background command produced no output")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Foreground commands run in their own session, detached from the CLI's
// controlling terminal, so /dev/tty fails instead of stopping the command.
func TestShellOwnSession(t *testing.T) {
	e := setup(t)
	expectStdout(t, e, `awk '{print ($6 == $1) ? "leader" : "member"}' /proc/$$/stat`, "leader\n")
}

// Invalid output bytes become one U+FFFD per maximal invalid subsequence, as
// Node's decoder writes them.
func TestShellOutputReplacement(t *testing.T) {
	e := setup(t)
	expectStdout(t, e, `printf 'a\377\376b\342\202c'`, "a��b�c")
}

// Like the desktop, a stream stops reaching the agent after 1 MiB; the
// command still runs to its end.
func TestShellOutputCap(t *testing.T) {
	e := setup(t)
	r := runShell(t, e, `head -c 2000000 /dev/zero | tr '\0' a; echo done >&2`)
	if len(r.stdout) > 1<<20 || len(r.stdout) == 0 || strings.Trim(r.stdout, "a") != "" || r.stderr != "done\n" || r.exit.Code != 0 {
		t.Fatalf("stdout %d bytes, stderr %q, exit %v", len(r.stdout), r.stderr, r.exit)
	}
}

// A shell killed by a terminating signal of its own takes the jobs it started
// with it, as on the desktop since 1.0.1 (Np), instead of leaving them to
// hold its output open; an aborted one is killed as before.
func TestShellKilledBySignalTakesItsJobs(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc")
	}
	eachShell(t, func(t *testing.T, home string) {
		e := setupHome(t, home)
		start := time.Now()
		r := runShell(t, e, `sleep 30 & echo $!; kill -KILL $$`)
		assert.Less(t, time.Since(start), 4*time.Second, "waited for the job's output")
		pid, err := strconv.Atoi(strings.TrimSpace(r.stdout))
		require.NoError(t, err, r.stdout)
		assert.Eventually(t, func() bool {
			b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
			if err != nil {
				return true
			}
			_, state, _ := strings.Cut(string(b), ") ")
			return strings.HasPrefix(state, "Z")
		}, 5*time.Second, 10*time.Millisecond, "job survived its shell")
	})
}
