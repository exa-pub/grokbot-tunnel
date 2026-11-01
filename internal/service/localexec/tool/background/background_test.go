package background_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/shellsession"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/terminalfile"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/background"
)

func newBackground(t *testing.T) (*background.Executor, *pathroot.Root) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/bash")
	root, err := pathroot.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { root.Close() })
	return background.New(root, shellsession.New(root)), root
}

func waitEnded(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		b, _ := os.ReadFile(path)
		// The footer comes first, then the status, as on the desktop.
		if strings.Contains(string(b), "\nended_at: ") && !strings.Contains(string(b), terminalfile.StatusField(terminalfile.Running)) {
			return string(b)
		}
		require.True(t, time.Now().Before(deadline), "terminal not finished: %q", b)
		time.Sleep(10 * time.Millisecond)
	}
}

// Execute reports the shell ID; the output goes to terminals/<id>.txt between
// the header and the footer.
func TestExecuteWritesTerminalFile(t *testing.T) {
	bg, root := newBackground(t)
	var got *agentv1.ExecClientMessage
	err := bg.Execute(context.Background(), &agentv1.BackgroundShellSpawnArgs{Command: "printf ran; exit 3"},
		func(m *agentv1.ExecClientMessage) error { got = m; return nil })
	require.NoError(t, err)
	success := got.GetBackgroundShellSpawnResult().GetSuccess()
	require.NotNil(t, success)
	assert.GreaterOrEqual(t, success.ShellId, uint32(1000))
	assert.LessOrEqual(t, success.ShellId, uint32(999_999))
	assert.Equal(t, root.Dir(), success.WorkingDirectory)
	assert.NotZero(t, success.GetPid())
	data := waitEnded(t, filepath.Join(root.Dir(), terminalfile.Dir, fmt.Sprintf("%d.txt", success.ShellId)))
	assert.True(t, strings.HasPrefix(data, "---\npid: "))
	assert.Contains(t, data, "---\nran\n---\nexit_code: 3\nelapsed_ms: ")
	assert.Contains(t, data, terminalfile.StatusField(terminalfile.Failed))
}

// D10: like the desktop, shell IDs are consecutive in 1000..999999 from a
// random start, skip only the IDs of running commands and overwrite old
// terminal files.
func TestTerminalIDs(t *testing.T) {
	bg, root := newBackground(t)
	next := func() uint32 {
		t.Helper()
		n, f, err := bg.Terminal()
		require.NoError(t, err)
		require.NoError(t, f.Close())
		assert.GreaterOrEqual(t, n, uint32(1000))
		assert.LessOrEqual(t, n, uint32(999_999))
		return n
	}
	a := next()
	b := next()
	assert.True(t, b == a+1 || a == 999_999 && b == 1000, "%d after %d", b, a)
	old := filepath.Join(root.Dir(), terminalfile.Dir, fmt.Sprintf("%d.txt", b+1))
	require.NoError(t, os.WriteFile(old, []byte("old"), 0o600))
	if b < 999_999 {
		assert.Equal(t, b+1, next())
		data, err := os.ReadFile(old)
		require.NoError(t, err)
		assert.Empty(t, data)
	}
	bg.Release(a)
	assert.NotEqual(t, a, next())
}
