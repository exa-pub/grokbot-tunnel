package shell

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/shellsession"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/background"
)

func newShell(t *testing.T) (*Executor, *pathroot.Root) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root, err := pathroot.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { root.Close() })
	session, err := shellsession.New(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return New(session, background.New(root, session)), root
}

// A command that finished is not reported as aborted, even when its context
// ends at the same time.
func TestShellFinishedCommandNotAborted(t *testing.T) {
	ex, root := newShell(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 20 {
		var exit *agentv1.ShellStreamExit
		send := func(m *agentv1.ExecClientMessage) error {
			if m.GetShellStream().GetExit() != nil {
				exit = m.GetShellStream().GetExit()
			}
			return nil
		}
		sh, err := ex.startShell(&agentv1.ShellArgs{Command: "true", Timeout: 1}, root.Dir(), shellsession.State{}, time.Now(), send)
		if err != nil {
			t.Fatal(err)
		}
		<-sh.done
		if err := sh.wait(ctx); err != nil {
			t.Fatal(err)
		}
		proc.CloseLogged(sh.snapshot)
		proc.CloseLogged(sh.fallback)
		if exit == nil || exit.Aborted {
			t.Fatal(exit)
		}
	}
}

// Output split inside a UTF-8 sequence is held back until the sequence is
// complete.
func TestStreamWriterKeepsUTF8Whole(t *testing.T) {
	var got strings.Builder
	o := &shellOutput{send: func(m *agentv1.ExecClientMessage) error {
		got.WriteString(m.GetShellStream().GetStdout().GetData())
		return nil
	}}
	w := &streamWriter{out: o, event: stdoutEvent}
	data := []byte("hello h\u00e9llo \u4e16\u754c \U0001F30D") // 2-, 3- and 4-byte UTF-8 sequences
	for _, b := range data {
		w.Write([]byte{b})
	}
	w.flush()
	if got.String() != string(data) {
		t.Fatalf("broken utf8: %q", got.String())
	}
}

// Like the desktop's D3, the full output stops at 49 MiB, flushing a cut
// sequence, and its file ends with a note that counts towards its size and
// lines; the agent's stream is not affected.
func TestFullOutputTruncated(t *testing.T) {
	root, err := pathroot.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	full := &fullOutput{root: root, threshold: 1}
	var got strings.Builder
	o := &shellOutput{full: full, send: func(m *agentv1.ExecClientMessage) error {
		got.WriteString(m.GetShellStream().GetStdout().GetData())
		return nil
	}}
	w := &streamWriter{out: o, event: stdoutEvent}
	w.Write([]byte("ab\n\xe4"))
	full.size = maxFullOutput - 2 // as if the output so far had been kept
	w.Write([]byte("\xb8\x96c"))
	w.Write([]byte("d\n"))
	w.flush()
	location, note := full.close()
	if note != "" || location == nil {
		t.Fatal(location, note)
	}
	data, err := os.ReadFile(location.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	const trailer = "\n... (output truncated at 51380222 bytes) ...\n" // 49 MiB - 2
	if string(data) != "ab\n�"+trailer || location.SizeBytes != maxFullOutput-2+int64(len(trailer)) || location.LineCount != 3 {
		t.Fatalf("%q %v", data, location)
	}
	if got.String() != "ab\n世cd\n" {
		t.Fatalf("agent got %q", got.String())
	}
}

// The soft timeout is chosen like the desktop's Nye.
func TestSoftTimeout(t *testing.T) {
	hard := int32(1000)
	for _, tc := range []struct {
		args *agentv1.ShellArgs
		want int32
	}{
		{&agentv1.ShellArgs{}, 30_000},
		{&agentv1.ShellArgs{Timeout: 500}, 500},
		{&agentv1.ShellArgs{Timeout: 500, IsBackground: true}, 500},
		{&agentv1.ShellArgs{IsBackground: true}, 0},
		{&agentv1.ShellArgs{TimeoutBehavior: agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND}, 0},
		{&agentv1.ShellArgs{HardTimeout: &hard}, 0},
		{&agentv1.ShellArgs{TimeoutBehavior: agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_CANCEL}, 30_000},
	} {
		if got := softTimeout(tc.args); got != tc.want {
			t.Errorf("softTimeout(%v) = %d, want %d", tc.args, got, tc.want)
		}
	}
}
