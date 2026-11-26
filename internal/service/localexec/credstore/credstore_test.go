package credstore_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/credstore"
)

func store(home string, env map[string]string, now func() time.Time) *credstore.Store {
	return credstore.New(credstore.Options{
		Getenv: func(k string) string { return env[k] },
		Home:   func() string { return home },
		Now:    now,
	})
}

// TestTable checks paths against the desktop's table (AAe), by name only:
// none of them exists.
func TestTable(t *testing.T) {
	home := t.TempDir()
	xdg := t.TempDir()
	serve := t.TempDir()
	molten := t.TempDir()
	s := store(home, map[string]string{
		"XDG_CONFIG_HOME": xdg, "AGENT_SERVE_CONFIG_DIR": serve, "MOLTEN_GLASS_USER_DATA": molten,
	}, nil)
	blocked := []string{
		".config/cursor/auth.json", ".config/cursor-dev/auth.json", ".cursor/auth.json", ".cursor-nightly/auth.json",
		".cursor-/auth.json", ".config/gb/acct/auth.json", ".gb/acct/auth.json", ".cursor/sdk/auth.json",
		".config/agent-serve/credentials.json", ".config/agent-serve/mcp-auth.json", ".cursor/projects/p/mcp-auth.json",
		".cursor-agents/mcp/mcp-auth.json", ".cursor-agents-2/backends/b/mcp/mcp-auth.json",
		".grok/agent-host-daemon/mcp/mcp-auth.json", ".grok/agent-host-daemon-x/backends/y/mcp/mcp-auth.json",
		".molten-glass/auth.json", ".molten-glass-dev/byok.json", ".molten-glass/byok-.json", ".molten-glass/byok-openai.json",
		".grok/build-desktop/self-hosted-worker.json", ".molten-glass/plugin-mcp/mcp-auth-x.json",
		".config/Cursor/molten/auth.json", ".config/Cursor Nightly/molten/auth.json", ".config/code-oss-dev/molten/byok.json",
		".config/Molten Glass/auth.json", ".config/Molten Glass 2/auth.json",
		// Every name also with a suffix, or hidden with one (Wht).
		".cursor/auth.json.bak", ".cursor/.auth.json.tmp", ".cursor/.auth.json.",
	}
	for _, p := range blocked {
		assert.True(t, s.Blocked(filepath.Join(home, p)), p)
	}
	for _, p := range []string{"cursor/auth.json", "gb/a/auth.json", "Molten Glass/auth.json"} {
		assert.True(t, s.Blocked(filepath.Join(xdg, p)), "XDG "+p)
	}
	for _, p := range []string{"credentials.json", "mcp-auth.json"} {
		assert.True(t, s.Blocked(filepath.Join(serve, p)), "agent-serve "+p)
	}
	assert.True(t, s.Blocked(filepath.Join(molten, "byok.json")))

	allowed := []string{
		".cursor/other.json", ".cursor/auth.jsonx", ".cursor/.auth.json", ".cursor/xauth.json", ".gb/auth.json",
		".gb/a/b/auth.json", ".cursor/mcp-auth.json", ".cursor/projects/mcp-auth.json", ".grok/auth.json",
		".grok/agent-host-daemon/mcp-auth.json", ".config/Cursor/auth.json", ".molten-glass/byok.jsonx",
		".config/cursor", "auth.json", ".cursor", ".Cursor/auth.json",
	}
	for _, p := range allowed {
		assert.False(t, s.Blocked(filepath.Join(home, p)), p)
	}
	assert.False(t, s.Blocked("relative/.cursor/auth.json"))
	assert.True(t, s.Blocked(filepath.Join(home, ".cursor", "x", "..", "auth.json")), "cleaned")
}

// TestEnv: a variable counts only if it names an absolute path.
func TestEnv(t *testing.T) {
	home := t.TempDir()
	s := store(home, map[string]string{"XDG_CONFIG_HOME": "rel", "AGENT_SERVE_CONFIG_DIR": "rel"}, nil)
	assert.False(t, s.Blocked(filepath.Join(home, "rel", "cursor", "auth.json")))
	assert.True(t, s.Blocked(filepath.Join(home, ".config", "cursor", "auth.json")))
	assert.False(t, store("", nil, nil).Blocked("/.cursor/auth.json"), "no home, no patterns")
}

// TestLinks: a credential directory that is a link counts where it leads,
// and a link to a credential file counts as the file.
func TestLinks(t *testing.T) {
	home, elsewhere := t.TempDir(), t.TempDir()
	for _, d := range []string{"cursor", "gb/acct", "grok/agent-host-daemon-dev/backends/b1/mcp"} {
		require.NoError(t, os.MkdirAll(filepath.Join(elsewhere, d), 0o755))
	}
	require.NoError(t, os.Symlink(filepath.Join(elsewhere, "cursor"), filepath.Join(home, ".cursor")))
	require.NoError(t, os.Symlink(filepath.Join(elsewhere, "gb"), filepath.Join(home, ".gb")))
	require.NoError(t, os.Symlink(filepath.Join(elsewhere, "grok"), filepath.Join(home, ".grok")))
	require.NoError(t, os.WriteFile(filepath.Join(elsewhere, "cursor", "auth.json"), nil, 0o600))
	require.NoError(t, os.Symlink(filepath.Join(elsewhere, "cursor", "auth.json"), filepath.Join(elsewhere, "alias")))
	s := store(home, nil, nil)

	assert.True(t, s.Blocked(filepath.Join(elsewhere, "cursor", "auth.json")))
	assert.True(t, s.Blocked(filepath.Join(elsewhere, "cursor", "projects", "p", "mcp-auth.json")), "missing below a link")
	assert.True(t, s.Blocked(filepath.Join(elsewhere, "gb", "acct", "auth.json")))
	assert.True(t, s.Blocked(filepath.Join(elsewhere, "grok", "agent-host-daemon-dev", "backends", "b1", "mcp", "mcp-auth.json")))
	assert.True(t, s.Blocked(filepath.Join(elsewhere, "alias")), "a link to a credential file")
	assert.False(t, s.Blocked(filepath.Join(elsewhere, "cursor", "other.json")))
	assert.False(t, s.Blocked(filepath.Join(elsewhere, "gb", "auth.json")))
}

// TestRebuild: links made after the mappings were built count once they
// are 30 s old, as the desktop rebuilds them.
func TestRebuild(t *testing.T) {
	home, elsewhere := t.TempDir(), t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(elsewhere, "cursor"), 0o755))
	now := time.Unix(1000, 0)
	s := store(home, nil, func() time.Time { return now })
	target := filepath.Join(elsewhere, "cursor", "auth.json")
	assert.False(t, s.Blocked(target))
	require.NoError(t, os.Symlink(filepath.Join(elsewhere, "cursor"), filepath.Join(home, ".cursor")))
	now = now.Add(29 * time.Second)
	assert.False(t, s.Blocked(target), "mappings still fresh")
	now = now.Add(time.Second)
	assert.True(t, s.Blocked(target))
}
