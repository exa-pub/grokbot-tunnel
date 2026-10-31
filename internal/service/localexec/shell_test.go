package localexec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestQueuedShellCancelsWhileWaiting(t *testing.T) {
	e := setup(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		collect(t, e, execFrame(`{"shellStreamArgs":{"command":"sleep 1"}}`))
	}()
	time.Sleep(100 * time.Millisecond) // let the first command take the shell
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := e.Handle(ctx, execFrame(`{"shellStreamArgs":{"command":"true"}}`), false,
		func(*aiserverv1.GrokBotUserComputerResponseFrame) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("canceled command waited for the running one")
	}
	<-done
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
