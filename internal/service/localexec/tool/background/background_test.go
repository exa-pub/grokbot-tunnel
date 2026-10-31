package background_test

import (
	"context"
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
		if terminalfile.Ended(b) {
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
	assert.Equal(t, uint32(1), success.ShellId)
	assert.Equal(t, root.Dir(), success.WorkingDirectory)
	assert.NotZero(t, success.GetPid())
	data := waitEnded(t, filepath.Join(root.Dir(), terminalfile.Dir, "1.txt"))
	assert.True(t, strings.HasPrefix(data, "---\npid: "))
	assert.Contains(t, data, "---\nran\n---\nexit_code: 3\n")
	assert.Contains(t, data, terminalfile.StatusField("failed"))
}

// Terminal skips numbers whose files already exist.
func TestTerminalSkipsExistingFiles(t *testing.T) {
	bg, root := newBackground(t)
	require.NoError(t, os.MkdirAll(filepath.Join(root.Dir(), terminalfile.Dir), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root.Dir(), terminalfile.Dir, "1.txt"), nil, 0o600))
	n, f, err := bg.Terminal()
	require.NoError(t, err)
	f.Close()
	assert.Equal(t, uint32(2), n)
}
