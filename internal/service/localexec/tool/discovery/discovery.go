// Package discovery summarizes the setup and recent activity of AI coding
// tools on the computer (Claude Code, Codex), like the desktop's AI tool
// discovery: names of MCP servers, plugins, skills and commands and redacted
// recent requests, never file contents.
package discovery

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/frames"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
)

const (
	discoveryTimeout = 10 * time.Second
	historyBytes     = 256 << 10 // read from the end of history.jsonl
	maxConfigBytes   = pathroot.MaxFile
	maxDirEntries    = 200
	maxSessionFiles  = 500
	maxSetupItems    = 24
	maxItems         = 40
	maxTitleUnits    = 160 // UTF-16 code units, like the desktop
)

var errUnsupportedSource = errors.New("unsupported AI tool source")

type discoveryItem struct {
	Kind         string `json:"kind"`
	Title        string `json:"title"`
	Excerpt      string `json:"excerpt,omitempty"`
	OccurredAtMS *int64 `json:"occurredAtMs,omitempty"`
}

// at is the item's time for sorting, 0 without one.
func (i discoveryItem) at() int64 {
	if i.OccurredAtMS == nil {
		return 0
	}
	return *i.OccurredAtMS
}

type discoveryResult struct {
	Source         string          `json:"source"`
	Found          bool            `json:"found"`
	SessionCount   int             `json:"sessionCount"`
	LastActiveAtMS *int64          `json:"lastActiveAtMs,omitempty"`
	Items          []discoveryItem `json:"items"`
}

// Config locates the tools' files; empty fields mean $HOME, $CODEX_HOME and
// $CLAUDE_CONFIG_DIR.
type Config struct {
	Home            string
	CodexHome       string
	ClaudeConfigDir string
}

// Discoverer discovers AI tool setups.
type Discoverer struct {
	cfg Config
}

// New returns a Discoverer for the files that cfg locates.
func New(cfg Config) *Discoverer { return &Discoverer{cfg: cfg} }

// discoveryLayout locates one tool's files.
type discoveryLayout struct {
	root         string // configuration directory
	global       string // global configuration file, if any
	promptDir    string // custom commands, relative to root
	promptDepth  int
	sessionDir   string // session logs, relative to root
	sessionDepth int
}

func envOverride(value, key string) string {
	if value == "" {
		value = os.Getenv(key)
	}
	return strings.TrimSpace(value)
}

// found reports whether the tool's configuration exists.
func (l discoveryLayout) found() (bool, error) {
	if found, err := exists(l.root, true); found || err != nil {
		return found, err
	}
	if l.global == "" {
		return false, nil
	}
	return exists(l.global, false)
}

// Discover summarizes an AI tool's setup and recent activity as the JSON the
// desktop sends: names of MCP servers, plugins, skills and commands plus
// redacted recent requests. File contents beyond these names are never
// included.
func (dd *Discoverer) Discover(ctx context.Context, source string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	layout, err := dd.discoveryLayout(source)
	if err != nil {
		return nil, err
	}
	result := discoveryResult{Source: source, Items: []discoveryItem{}}
	if result.Found, err = layout.found(); err != nil {
		return nil, err
	}
	if !result.Found {
		return json.Marshal(result)
	}
	d := &scan{ctx: ctx, source: source, layout: layout}
	if err = d.collect(); err != nil {
		return nil, err
	}
	setup := dedupItems(d.setup)
	setup = setup[:min(len(setup), maxSetupItems)]
	// Like the desktop (KJe), a repeated request keeps its first, oldest
	// entry; then the newest come first.
	sessions := dedupItems(d.sessions)
	slices.SortStableFunc(sessions, func(a, b discoveryItem) int { return cmp.Compare(b.at(), a.at()) })
	sessions = sessions[:min(len(sessions), maxItems-len(setup))]
	result.Items = append(setup, sessions...)
	result.SessionCount = max(d.sessionFiles, len(sessions))
	result.LastActiveAtMS = d.lastActive
	if len(sessions) > 0 && sessions[0].OccurredAtMS != nil &&
		(result.LastActiveAtMS == nil || *sessions[0].OccurredAtMS > *result.LastActiveAtMS) {
		result.LastActiveAtMS = sessions[0].OccurredAtMS
	}
	return json.Marshal(result)
}

func (dd *Discoverer) discoveryLayout(source string) (discoveryLayout, error) {
	home := dd.cfg.Home
	if home == "" {
		var err error
		if home, err = os.UserHomeDir(); err != nil {
			return discoveryLayout{}, err
		}
	}
	switch source {
	case frames.SourceCodex:
		l := discoveryLayout{root: filepath.Join(home, ".codex"), promptDir: "prompts", sessionDir: "sessions", sessionDepth: 4}
		if dir := envOverride(dd.cfg.CodexHome, "CODEX_HOME"); dir != "" {
			l.root = dir
		}
		return l, nil
	case frames.SourceClaudeCode:
		l := discoveryLayout{
			root:         filepath.Join(home, ".claude"),
			global:       filepath.Join(home, ".claude.json"),
			promptDir:    "commands",
			promptDepth:  2,
			sessionDir:   "projects",
			sessionDepth: 2,
		}
		if dir := envOverride(dd.cfg.ClaudeConfigDir, "CLAUDE_CONFIG_DIR"); dir != "" {
			l.root = dir
			l.global = filepath.Join(dir, ".claude.json")
		}
		return l, nil
	}
	return discoveryLayout{}, errUnsupportedSource
}
