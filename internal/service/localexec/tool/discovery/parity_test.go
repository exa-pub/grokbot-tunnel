package discovery

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func discover(t *testing.T, e *Discoverer, source string) (discoveryResult, string) {
	t.Helper()
	b, err := e.Discover(context.Background(), source)
	require.NoError(t, err)
	var r discoveryResult
	require.NoError(t, json.Unmarshal(b, &r))
	return r, string(b)
}

// Setup items keep the order of the configuration files, as JavaScript
// enumerates them (F7c), and MCP fields that are not strings are left out
// instead of dropping the server (F7e).
func TestDiscoveryKeepsConfigOrder(t *testing.T) {
	e := discoverySetup(t)
	put(t, filepath.Join(e.cfg.ClaudeConfigDir, ".claude.json"), `{
		"projects": {"/p2": {"mcpServers": {"p2": {"type": "sse"}}}, "/p1": {"mcpServers": {"p1": {}}}},
		"mcpServers": {"zeta": {"command": 5, "url": "u"}, "alpha": {"command": "/bin/run/"}, "10": 5, "2": {"type": 1}}}`)
	put(t, filepath.Join(e.cfg.ClaudeConfigDir, "plugins", "installed_plugins.json"), `{"plugins": {"zz": {}, "aa": {}}}`)
	r, raw := discover(t, e, "claude_code")
	got := make([]string, 0, len(r.Items))
	for _, item := range r.Items {
		got = append(got, item.Title+"="+item.Excerpt)
	}
	assert.Equal(t, []string{"2=unknown", "10=unknown", "zeta=http", "alpha=stdio · run", "p2=sse", "p1=unknown", "zz=", "aa="}, got, raw)
}

// History times are taken as the desktop takes them: Claude Code's in
// milliseconds as they are, Codex's seconds below 10^12 times 1000, and a
// time of 0 is kept (F7f).
func TestDiscoveryHistoryTimes(t *testing.T) {
	e := discoverySetup(t)
	put(t, filepath.Join(e.cfg.ClaudeConfigDir, "history.jsonl"), strings.Join([]string{
		`{"display":"zero","timestamp":0}`, `{"display":"seconds","timestamp":1700000000}`, `{"display":"none"}`,
		`{"display":"bad","timestamp":"x"}`, `{"display":5}`,
	}, "\n"))
	r, raw := discover(t, e, "claude_code")
	times := map[string]any{}
	for _, item := range r.Items {
		times[item.Title] = nil
		if item.OccurredAtMS != nil {
			times[item.Title] = *item.OccurredAtMS
		}
	}
	assert.Equal(t, map[string]any{"zero": int64(0), "seconds": int64(1700000000), "none": nil}, times, raw)
	assert.Contains(t, raw, `"occurredAtMs":0`)
	require.NotNil(t, r.LastActiveAtMS)
	assert.Equal(t, int64(1700000000), *r.LastActiveAtMS)

	put(t, filepath.Join(e.cfg.CodexHome, "history.jsonl"), `{"text":"zero","ts":0}`+"\n"+`{"text":"later","ts":1700000000.5}`)
	r, raw = discover(t, e, "codex")
	require.Len(t, r.Items, 2, raw)
	assert.Equal(t, int64(1700000000500), r.Items[0].at())
	assert.Equal(t, "zero", r.Items[1].Title)
	assert.Contains(t, raw, `"occurredAtMs":0`)
}

// Titles are shortened like the desktop's, without the space before the
// ellipsis (F7a), and paths only match where they start a word (F7b).
func TestDiscoveryTitleLikeDesktop(t *testing.T) {
	assert.Equal(t, strings.Repeat("x", 158)+"…", discoveryTitle(strings.Repeat("x", 158)+" tail"))
	assert.Equal(t, "build src/main/java and [path]", discoveryTitle("build src/main/java and ./a/b"))
	assert.Equal(t, "ns:[path]:cmd", commandTitle([]string{"ns", "/home/me/x", "cmd"}))
}
