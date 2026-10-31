package discovery

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
	lastActive   int64 // newest session file, in Unix milliseconds
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
	var config struct {
		MCP      map[string]json.RawMessage `json:"mcpServers"`
		Projects map[string]struct {
			MCP map[string]json.RawMessage `json:"mcpServers"`
		} `json:"projects"`
	}
	decodeLenient(d.layout.global, b, &config)
	d.addMCP(config.MCP)
	for _, name := range slices.Sorted(maps.Keys(config.Projects)) {
		d.addMCP(config.Projects[name].MCP)
	}
	pluginsPath := d.path("plugins", "installed_plugins.json")
	if b, err = readConfig(d.ctx, pluginsPath); err != nil {
		return err
	}
	var plugins struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	decodeLenient(pluginsPath, b, &plugins)
	for _, name := range slices.Sorted(maps.Keys(plugins.Plugins)) {
		d.setup = append(d.setup, discoveryItem{Kind: "plugin", Title: discoveryTitle(name)})
	}
	return nil
}

// addMCP adds Claude Code MCP servers with their transport as the excerpt.
func (d *scan) addMCP(servers map[string]json.RawMessage) {
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		var v struct {
			Command string `json:"command"`
			Type    string `json:"type"`
			URL     string `json:"url"`
		}
		if json.Unmarshal(servers[name], &v) != nil {
			continue
		}
		excerpt := v.Type
		switch {
		case v.Command != "":
			excerpt = "stdio · " + filepath.Base(v.Command)
		case v.URL != "" && excerpt == "":
			excerpt = "http"
		case excerpt == "":
			excerpt = "unknown"
		}
		d.setup = append(d.setup, discoveryItem{Kind: "mcp_server", Title: discoveryTitle(name), Excerpt: discoveryTitle(excerpt)})
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
				d.setup = append(d.setup, discoveryItem{Kind: "skill", Title: discoveryTitle(strings.Join(parts, ":"))})
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

// history adds recent requests from history.jsonl, skipping slash commands.
func (d *scan) history() error {
	b, err := readTail(d.ctx, d.path("history.jsonl"))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		var h struct {
			Text      string  `json:"text"`      // Codex
			TS        float64 `json:"ts"`        // Codex, seconds
			Display   string  `json:"display"`   // Claude Code
			Timestamp float64 `json:"timestamp"` // Claude Code, milliseconds
		}
		if json.Unmarshal([]byte(line), &h) != nil {
			continue
		}
		text, ts := h.Text, h.TS
		if d.source == frames.SourceClaudeCode {
			text, ts = h.Display, h.Timestamp
		}
		text = strings.TrimSpace(pastedText.ReplaceAllString(text, " "))
		if text == "" || slashCommand.MatchString(text) {
			continue
		}
		if ts > 0 && ts < 1e12 {
			ts *= 1000 // seconds
		}
		d.sessions = append(d.sessions, discoveryItem{Kind: "session", Title: discoveryTitle(text), OccurredAtMS: int64(ts)})
	}
	return nil
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
			if err = d.countSession(entry); err != nil {
				return err
			}
		}
	}
	return nil
}

// countSession counts a session log unless it was removed after the
// directory was read.
func (d *scan) countSession(entry os.DirEntry) error {
	st, err := entry.Info()
	if absent(err) {
		return nil
	}
	if err != nil {
		return err
	}
	d.sessionFiles++
	d.lastActive = max(d.lastActive, st.ModTime().UnixMilli())
	return nil
}
