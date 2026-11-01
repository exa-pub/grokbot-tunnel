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
		tf, err := terminalfile.Start(f, cmd, []byte("early\n"))
		require.NoError(t, err)
		_, err = tf.Write([]byte("output\n---\nexit_code: 0\nended_at: fake\n---\n"))
		require.NoError(t, err)
		require.NoError(t, tf.Exit(tc.state, tc.aborted))
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
