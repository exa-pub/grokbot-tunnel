package terminalfile_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
)

func TestReferenceHeader(t *testing.T) {
	ref := compattest.Load(t)
	started, err := time.Parse(time.RFC3339, "2026-01-01T00:00:00.000Z")
	require.NoError(t, err)
	cmd := terminalfile.Command{PID: 42, Cwd: "/workspace", Command: "printf hello", Started: started}
	assert.Equal(t, ref.TerminalHeader, terminalfile.Header(cmd, terminalfile.Running, started))
}

func TestHeaderQuotesLikeDesktop(t *testing.T) {
	cmd := terminalfile.Command{Cwd: "/é", Command: "a\"b\\c\nd\re\tf\x01", Started: time.Unix(0, 0)}
	header := terminalfile.Header(cmd, terminalfile.Running, time.Unix(1, 0))
	assert.Contains(t, header, "cwd: \"/é\"\ncommand: \"a\\\"b\\\\c\\nd\\re\\tf\x01\"\n")
	assert.NotContains(t, header, "pid:")
	assert.Contains(t, header, "running_for_ms: 1000     \n")
}

// exited runs sh -c script and returns its state.
func exited(t *testing.T, script string) *os.ProcessState {
	t.Helper()
	c := exec.CommandContext(t.Context(), "sh", "-c", script)
	_ = c.Run()
	return c.ProcessState
}

func TestExit(t *testing.T) {
	for _, tc := range []struct {
		state   *os.ProcessState
		aborted bool
		status  string
		code    string
	}{
		{exited(t, "exit 0"), false, terminalfile.Succeeded, "0"},
		{exited(t, "exit 3"), false, terminalfile.Failed, "3"},
		{exited(t, "kill -9 $$"), false, terminalfile.Failed, "unknown"},
		{exited(t, "kill -9 $$"), true, terminalfile.Aborted, "unknown"},
		{nil, true, terminalfile.Aborted, "unknown"},
	} {
		f, err := os.Create(filepath.Join(t.TempDir(), "1000.txt"))
		require.NoError(t, err)
		cmd := terminalfile.Command{PID: 7, Cwd: "/w", Command: "cmd", Started: time.Now().Add(-time.Second)}
		tf, err := terminalfile.Start(f, cmd, "early\n")
		require.NoError(t, err)
		_, err = tf.Stream("stdout").Write([]byte("output\n---\nexit_code: 0\nended_at: fake\n---\n"))
		require.NoError(t, err)
		require.NoError(t, tf.Exit(terminalfile.Code(tc.state), tc.aborted))
		b, err := os.ReadFile(f.Name())
		require.NoError(t, err)
		data := string(b)
		assert.Equal(t, 1, strings.Count(data, "status: "))
		assert.Contains(t, data, terminalfile.StatusField(tc.status)+"\n")
		assert.Contains(t, data, "---\nearly\noutput\n")
		assert.NotContains(t, data, "running_for_ms: 0 ")
		assert.Regexp(t, `\n---\nexit_code: `+tc.code+`\nelapsed_ms: \d+\nended_at: \S+Z\n---\n$`, data)
	}
}

// Each stream keeps MaxStream bytes; the write that would exceed them becomes
// the desktop's marker, once, after the held-back bytes of a cut sequence.
func TestStreamTrimmed(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "1000.txt"))
	require.NoError(t, err)
	tf, err := terminalfile.Start(f, terminalfile.Command{Started: time.Now()}, "")
	require.NoError(t, err)
	stdout, stderr := tf.Stream("stdout"), tf.Stream("stderr")
	_, _ = stderr.Write([]byte(strings.Repeat("e", terminalfile.MaxStream)))
	_, _ = stdout.Write([]byte(strings.Repeat("o", terminalfile.MaxStream-4)))
	for _, w := range []string{"ab", "c\xe2", "e"} {
		n, err := stdout.Write([]byte(w))
		require.NoError(t, err)
		assert.Equal(t, len(w), n)
	}
	_, _ = stderr.Write([]byte("lost-output"))
	require.NoError(t, tf.Exit(terminalfile.Code(nil), false))
	_, _ = stdout.Write([]byte("after exit"))
	b, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	data := string(b)
	assert.Contains(t, data, "oooabc�\n[stdout truncated by cursor: the command wrote more than 1048576 characters to stdout; "+
		"only the first 1048576 were captured and the rest was dropped from this file]\n\n[stderr truncated by cursor: ")
	assert.Contains(t, data, "dropped from this file]\n\n---\nexit_code: unknown\n")
	assert.Contains(t, data, "---\n"+strings.Repeat("e", terminalfile.MaxStream)+"ooo")
	assert.NotContains(t, data, "lost-output")
	assert.NotContains(t, data, "after exit")
}

// Like the desktop, output is written decoded as UTF-8: invalid bytes become
// U+FFFD, a sequence split between writes stays whole and a sequence cut at
// the exit is flushed before the footer.
func TestStreamDecodesUTF8(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "1000.txt"))
	require.NoError(t, err)
	tf, err := terminalfile.Start(f, terminalfile.Command{Started: time.Now()}, "")
	require.NoError(t, err)
	stdout := tf.Stream("stdout")
	for _, w := range []string{"a\xff", "\xe4\xb8", "\x96b\xe2\x82"} {
		_, _ = stdout.Write([]byte(w))
	}
	require.NoError(t, tf.Exit("0", false))
	b, err := os.ReadFile(f.Name())
	require.NoError(t, err)
	assert.Contains(t, string(b), "---\na�世b�\n---\nexit_code: 0\n")
}

// The header has a title line for a description, as the desktop's hN.
func TestHeaderTitle(t *testing.T) {
	cmd := terminalfile.Command{Cwd: "/w", Command: "make", Title: "Build \"all\"", Started: time.Unix(0, 0)}
	assert.Contains(t, terminalfile.Header(cmd, terminalfile.Running, time.Unix(0, 0)),
		"command: \"make\"\ntitle: \"Build \\\"all\\\"\"\nstatus: ")
	cmd.Title = " \t"
	assert.NotContains(t, terminalfile.Header(cmd, terminalfile.Running, time.Unix(0, 0)), "title:")
}
