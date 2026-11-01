package discovery

import (
	"context"
	"log"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
)

// scan collects items for one tool.
type scan struct {
	ctx          context.Context
	source       string
	layout       discoveryLayout
	setup        []discoveryItem
	sessions     []discoveryItem
	sessionFiles int
	lastActive   *int64 // newest session file, in Unix milliseconds
}

func (d *scan) collect() error {
	steps := []func() error{d.mcpAndPlugins, d.skills, d.commands, d.history, d.countSessions}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

func (d *scan) path(elem ...string) string {
	return filepath.Join(append([]string{d.layout.root}, elem...)...)
}

// parseConfig reads a JSON object; a malformed file counts as empty.
func parseConfig(path string, b []byte) ([]member, bool) {
	if len(b) == 0 {
		return nil, false
	}
	members, ok := objectMembers(b)
	if !ok {
		log.Printf("discovery: ignoring malformed %s", path)
	}
	return members, ok
}

// mcpAndPlugins lists MCP servers and plugins in the order of the
// configuration files: global servers, the servers of each project, then
// plugins, like the desktop (CFe).
func (d *scan) mcpAndPlugins() error {
	if d.source == frames.SourceCodex {
		b, err := readConfig(d.ctx, d.path("config.toml"))
		if err != nil {
			return err
		}
		d.setup = append(d.setup, codexMCP(string(b))...)
		return nil
	}
	b, err := readConfig(d.ctx, d.layout.global)
	if err != nil {
		return err
	}
	if config, ok := parseConfig(d.layout.global, b); ok {
		d.addMCP(field(config, "mcpServers"))
		projects, _ := objectMembers(field(config, "projects"))
		for _, project := range projects {
			settings, _ := objectMembers(project.value)
			d.addMCP(field(settings, "mcpServers"))
		}
	}
	pluginsPath := d.path("plugins", "installed_plugins.json")
	if b, err = readConfig(d.ctx, pluginsPath); err != nil {
		return err
	}
	config, ok := parseConfig(pluginsPath, b)
	plugins, isObject := objectMembers(field(config, "plugins"))
	if !ok || field(config, "plugins") != nil && !isObject {
		return nil // the desktop refuses the whole file
	}
	for _, plugin := range plugins {
		d.setup = append(d.setup, discoveryItem{Kind: "plugin", Title: discoveryTitle(plugin.key)})
	}
	return nil
}

// addMCP adds Claude Code MCP servers with their transport as the excerpt
// (IFe, SGt). Fields that are not strings are left out, as zod's catch
// does.
func (d *scan) addMCP(raw []byte) {
	servers, _ := objectMembers(raw)
	for _, server := range servers {
		fields, _ := objectMembers(server.value)
		command, _ := stringField(fields, "command")
		kind, _ := stringField(fields, "type")
		url, _ := stringField(fields, "url")
		excerpt := kind
		switch {
		case command != "":
			excerpt = commandExcerpt(command)
		case kind != "":
		case url != "":
			excerpt = "http"
		default:
			excerpt = "unknown"
		}
		d.setup = append(d.setup, discoveryItem{Kind: "mcp_server", Title: discoveryTitle(server.key), Excerpt: excerpt})
	}
}

// skills lists skills/<name> directories, or symlinks to them, that contain
// a SKILL.md. Other entries are skipped.
func (d *scan) skills() error {
	entries, err := discoveryEntries(d.ctx, d.path("skills"), false)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		dir := d.path("skills", entry.Name())
		ok, err := entryIs(dir, entry, true)
		if err == nil && ok {
			ok, err = exists(filepath.Join(dir, "SKILL.md"), false)
		}
		if err != nil {
			return err
		}
		if ok {
			d.setup = append(d.setup, discoveryItem{Kind: "skill", Title: discoveryTitle(entry.Name())})
		}
	}
	return nil
}

// commands lists custom commands as "namespace:name".
func (d *scan) commands() error {
	return d.commandDir(d.path(d.layout.promptDir), d.layout.promptDepth, nil)
}

func (d *scan) commandDir(dir string, depth int, namespace []string) error {
	entries, err := discoveryEntries(d.ctx, dir, false)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		name, isMarkdown := strings.CutSuffix(entry.Name(), ".md")
		if isMarkdown {
			file, err := entryIs(path, entry, false)
			if err != nil {
				return err
			}
			if file {
				parts := append(slices.Clone(namespace), name)
				d.setup = append(d.setup, discoveryItem{Kind: "skill", Title: commandTitle(parts)})
				continue
			}
		}
		if depth == 0 {
			continue
		}
		subdir, err := entryIs(path, entry, true)
		if err != nil {
			return err
		}
		if subdir {
			if err = d.commandDir(path, depth-1, append(slices.Clone(namespace), entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// history adds recent requests from history.jsonl, skipping slash commands
// (qFe, MFe). A line needs its text as a string and, if it has one, its time
// as a number: Codex's ts in seconds (or milliseconds from 10^12 on), Claude
// Code's timestamp in milliseconds.
func (d *scan) history() error {
	b, err := readTail(d.ctx, d.path("history.jsonl"))
	if err != nil {
		return err
	}
	textKey, timeKey := "text", "ts"
	if d.source == frames.SourceClaudeCode {
		textKey, timeKey = "display", "timestamp"
	}
	for _, line := range strings.Split(string(b), "\n") {
		entry, ok := objectMembers([]byte(line))
		if !ok {
			continue
		}
		text, ok := stringField(entry, textKey)
		ts, present, isNumber := numberField(entry, timeKey)
		if !ok || present && !isNumber {
			continue
		}
		text = jsTrim(pastedText.ReplaceAllString(text, " "))
		if text == "" || isSlashCommand(text) {
			continue
		}
		item := discoveryItem{Kind: "session", Title: discoveryTitle(text)}
		if present {
			if d.source == frames.SourceCodex && ts < 1e12 {
				ts *= 1000 // seconds
			}
			at := int64(math.Floor(ts))
			item.OccurredAtMS = &at
		}
		d.sessions = append(d.sessions, item)
	}
	return nil
}

// isSlashCommand matches /name followed by whitespace or the end (mGt).
func isSlashCommand(text string) bool {
	loc := slashCommand.FindStringIndex(text)
	if loc == nil {
		return false
	}
	r, _ := utf8.DecodeRuneInString(text[loc[1]:])
	return loc[1] == len(text) || jsSpace(r)
}

// countSessions counts the newest session logs and finds the latest activity.
func (d *scan) countSessions() error {
	return d.countSessionDir(d.path(d.layout.sessionDir), 0)
}

func (d *scan) countSessionDir(dir string, level int) error {
	entries, err := discoveryEntries(d.ctx, dir, true)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if d.sessionFiles >= maxSessionFiles {
			return nil
		}
		switch {
		case entry.IsDir():
			if level < d.layout.sessionDepth {
				if err = d.countSessionDir(filepath.Join(dir, entry.Name()), level+1); err != nil {
					return err
				}
			}
		case entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".jsonl"):
			if err = d.countSession(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// countSession counts a session log; like the desktop, one removed after its
// directory was read still counts but has no time.
func (d *scan) countSession(path string) error {
	d.sessionFiles++
	st, err := statPath(path)
	if absent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	at := st.ModTime().UnixMilli()
	if d.lastActive == nil || at > *d.lastActive {
		d.lastActive = &at
	}
	return nil
}
