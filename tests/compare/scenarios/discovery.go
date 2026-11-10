package scenarios

import (
	"fmt"
	"strings"

	. "github.com/exa-pub/grokbot-tunnel/tests/compare/scenario" //nolint:revive,staticcheck // scenarios read as a list of steps
)

// history is n lines of a history.jsonl with texts that must be redacted.
func history(n int, format string) string {
	var b strings.Builder
	for i := range n {
		text := "request " + strings.Repeat("g", i+1) + " user@example.com https://private.example/path " +
			"sk-123456789012345678901234567890 /home/private/file"
		fmt.Fprintf(&b, format+"\n", text, 1700000000+i)
	}
	return b.String()
}

var claudeSetup = []File{
	Text(".claude.json", `{
  "projects": {"/p2": {"mcpServers": {"p2": {"type": "sse"}}}, "/p1": {"mcpServers": {"p1": {}}}},
  "mcpServers": {"zeta": {"command": 5, "url": "u"}, "alpha": {"command": "/bin/run/"}, "10": 5, "2": {"type": 1}}
}`),
	Text(".claude/plugins/installed_plugins.json", `{"plugins": {"zz@market": [{"version": "1"}], "aa": {}}}`),
	Text(".claude/skills/review/SKILL.md", "---\nname: review\ndescription: private body\n---\nbody\n"),
	Text(".claude/skills/empty/README.md", "no skill here\n"),
	Text(".claude/commands/deploy.md", "private command body\n"),
	Text(".claude/commands/team/ship.md", "private\n"),
	Text(".claude/commands/notes.txt", "not a command\n"),
	Text(".claude/history.jsonl", history(50, `{"display":%q,"timestamp":%d000,"project":"/home/private/p"}`)+
		`{"display":"/slash command","timestamp":1800000000000}`+"\n"+`not json`+"\n"),
	Text(".claude/projects/-p1/a.jsonl", "{}\n"), Text(".claude/projects/-p1/b.jsonl", "{}\n"),
	Text(".claude/projects/-p2/c.jsonl", "{}\n"),
}

var codexSetup = []File{
	Text(".codex/config.toml", "model = \"x\"\n[mcp_servers.safe]\ncommand = \"/usr/bin/tool\"\nargs = [\"--token\", \"secret\"]\n"+
		"[mcp_servers.remote]\nurl = \"https://secret.example/token\"\n[mcp_servers.\"quoted name\"]\ncommand = \"npx\"\n"),
	Text(".codex/skills/skill/SKILL.md", "private body must not be sent"),
	Text(".codex/prompts/review.md", "private body"),
	Text(".codex/sessions/2026/01/02/a.jsonl", "{}"), Text(".codex/sessions/2026/01/02/b.jsonl", "{}"),
	Text(".codex/history.jsonl", history(50, `{"text":%q,"ts":%d}`)),
}

func discovers(id, about string, home []File, sources ...string) Scenario {
	s := Scenario{ID: id, About: about, Home: home}
	for i, source := range sources {
		s.Steps = append(s.Steps, Do(Discover("r"+string(rune('1'+i)), source)))
	}
	return s
}

var discovery = []Scenario{
	discovers("discovery/missing", "no AI tool is set up", nil, "claude_code", "codex"),
	discovers("discovery/unknown", "sources the client does not know", nil, "cursor", "", "CODEX"),
	discovers("discovery/claude", "Claude Code: servers, plugins, skills, commands and history", claudeSetup, "claude_code"),
	discovers("discovery/codex", "Codex: servers, skills, prompts and history", codexSetup, "codex"),
	discovers("discovery/empty", "configuration directories without content",
		[]File{Dir(".claude"), Dir(".codex"), Text(".claude.json", "{}")}, "claude_code", "codex"),
	discovers("discovery/damaged", "configuration that cannot be parsed",
		[]File{
			Text(".claude.json", "{not json"), Text(".claude/plugins/installed_plugins.json", "[]"),
			Text(".claude/history.jsonl", "\n\n{}\n"),
			Text(".codex/config.toml", "[mcp_servers\n"), Text(".codex/history.jsonl", `{"text":5}`+"\n"),
		}, "claude_code", "codex"),
	discovers("discovery/unreadable", "configuration the user may not read",
		[]File{
			Text(".claude.json", "{}").WithMode(0), Text(".claude/history.jsonl", "{}").WithMode(0),
			Text(".codex/config.toml", "").WithMode(0), {Path: ".codex/skills", Dir: true, Mode: 0o000},
		}, "claude_code", "codex"),
	discovers("discovery/titles", "how request texts become titles",
		[]File{Text(".codex/history.jsonl", strings.Join([]string{
			`{"text":"` + strings.Repeat("x", 158) + ` tail","ts":1}`,
			`{"text":"build src/main/java and ./a/b","ts":2}`,
			`{"text":"  spaces   and\ttabs\nand lines  ","ts":3}`,
			`{"text":"mail a@b.co and key ghp_abcdefghijklmnopqrstuvwxyz0123456789 done","ts":4}`,
			`{"text":"see https://example.com/x?y=1 and C:\\Users\\me\\file","ts":5}`,
			`{"text":"same","ts":6}`, `{"text":"same","ts":7}`, `{"text":"","ts":8}`,
			`{"text":"seconds","ts":1700000000}`, `{"text":"millis","ts":1700000000000}`, `{"text":"fraction","ts":1700000000.5}`,
			`{"text":"none"}`, `{"text":"zero","ts":0}`,
		}, "\n")+"\n")}, "codex"),
}
