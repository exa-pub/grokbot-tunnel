// Package credstore recognizes the credential files of Cursor and Grok Bot
// clients, which the desktop's local read executor refuses to read since 1.3
// (anchors: "credentialStoreBlock", "Unknown credential store base"). A path
// is a credential file if it, or its realpath, matches a pattern of the
// desktop's table (AAe); a directory of a pattern reached through a symlink
// counts as its named location (jAt, KAt).
package credstore

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// rebuildAfter is how long the desktop keeps its symlink mappings (HAt).
const rebuildAfter = 30 * time.Second

// Environment variables that move a base directory of the table (Zht).
const (
	envXDGConfig   = "XDG_CONFIG_HOME"
	envAgentServe  = "AGENT_SERVE_CONFIG_DIR"
	envMoltenGlass = "MOLTEN_GLASS_USER_DATA"
)

// base names where the files of an entry live (Vht). The desktop's
// "appdata" base names directories only on Windows; its entries are left
// out here.
type base string

const (
	baseHome       base = "home"
	baseConfig     base = "config"
	baseCursorData base = "cursor-data"
	baseDaemon     base = "daemon"
	baseMoltenData base = "molten-data"
)

// entry is one line of the desktop's table: a base, or a directory named by
// an environment variable, and a path below it whose segments may hold *.
type entry struct {
	base   base
	envDir string
	path   string
}

var entries = []entry{
	{base: baseConfig, path: "cursor/auth.json"},
	{base: baseConfig, path: "cursor-dev/auth.json"},
	{base: baseCursorData, path: "auth.json"},
	{base: baseConfig, path: "gb/*/auth.json"},
	{base: baseHome, path: ".gb/*/auth.json"},
	{base: baseCursorData, path: "sdk/auth.json"},
	{base: baseConfig, path: "agent-serve/credentials.json"},
	{envDir: envAgentServe, path: "credentials.json"},
	{base: baseConfig, path: "agent-serve/mcp-auth.json"},
	{envDir: envAgentServe, path: "mcp-auth.json"},
	{base: baseCursorData, path: "projects/*/mcp-auth.json"},
	{base: baseDaemon, path: "mcp/mcp-auth.json"},
	{base: baseMoltenData, path: "auth.json"},
	{base: baseMoltenData, path: "self-hosted-worker.json"},
	{base: baseMoltenData, path: "byok.json"},
	{base: baseMoltenData, path: "byok-*.json"},
	{base: baseMoltenData, path: "plugin-mcp/mcp-auth-*.json"},
}

// Prefixes below the home directory and the configuration directories
// (Jht, Uht, $ht, Ght, Fht).
var (
	cursorData = []string{".cursor", ".cursor-*"}
	daemonDirs = [][]string{
		{".cursor-agents"}, {".cursor-agents-*"},
		{".grok", "agent-host-daemon"}, {".grok", "agent-host-daemon-*"},
		{".molten-glass"}, {".molten-glass-*"},
	}
	daemonBackends = []string{"backends", "*"}
	moltenHome     = [][]string{{".molten-glass"}, {".molten-glass-*"}, {".grok", "build-desktop"}}
	moltenConfig   = [][]string{
		{"Cursor", "molten"}, {"Cursor *", "molten"}, {"code-oss-dev", "molten"},
		{"Molten Glass"}, {"Molten Glass *"},
	}
)

// Store answers whether a path is a credential file. Its zero value is not
// usable; New and Default return one.
type Store struct {
	getenv func(string) string
	home   func() string
	now    func() time.Time

	mu       sync.Mutex
	tableKey string
	table    *table
	links    *links
}

// Options replace the process environment, for tests.
type Options struct {
	Getenv func(string) string // default os.Getenv
	Home   func() string       // default: $HOME, else the user's home
	Now    func() time.Time    // default time.Now
}

// New returns a Store.
func New(o Options) *Store {
	s := &Store{getenv: o.Getenv, home: o.Home, now: o.Now}
	if s.getenv == nil {
		s.getenv = os.Getenv
	}
	if s.home == nil {
		s.home = func() string { return homeDir(s.getenv) }
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

var defaultStore = New(Options{})

// Blocked reports whether path names a credential file, by the process
// environment.
func Blocked(path string) bool { return defaultStore.Blocked(path) }

// Blocked reports whether the absolute path names a credential file: it, or
// its realpath, matches a pattern (G4).
func (s *Store) Blocked(path string) bool {
	t := filepath.Clean(path)
	tbl, lk := s.state()
	if lk.match(tbl, t) {
		return true
	}
	resolved, ok := realpath(t)
	return ok && lk.match(tbl, resolved)
}

// state returns the table for the current environment and the symlink
// mappings of its directories, built again after rebuildAfter or when the
// directories change (bAe, ZAt).
func (s *Store) state() (*table, *links) {
	home := s.home()
	if home != "" && !filepath.IsAbs(home) {
		if abs, err := filepath.Abs(home); err == nil {
			home = abs
		}
	}
	key := strings.Join([]string{home, s.getenv(envXDGConfig), s.getenv(envAgentServe), s.getenv(envMoltenGlass)}, "\x00")
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.table == nil || s.tableKey != key {
		s.tableKey, s.table = key, newTable(home, s.getenv)
	}
	if s.links == nil || s.links.key != s.table.rootsKey || stale(s.links.built, now) {
		s.links = buildLinks(s.table, now)
	}
	return s.table, s.links
}

// stale reports whether mappings built at built are rebuildAfter away from
// now, either way, as the desktop compares them.
func stale(built, now time.Time) bool {
	d := now.Sub(built)
	return d >= rebuildAfter || d <= -rebuildAfter
}

// homeDir is Node's os.homedir: $HOME, else the home of the user.
func homeDir(getenv func(string) string) string {
	if h := getenv("HOME"); h != "" {
		return h
	}
	if u, err := user.Current(); err == nil {
		return u.HomeDir
	}
	return ""
}

// absEnv is the desktop's FE: a variable naming an absolute path, if set.
func absEnv(getenv func(string) string, name string) (string, bool) {
	v := getenv(name)
	return v, v != "" && filepath.IsAbs(v)
}

// configDirs are $XDG_CONFIG_HOME, if absolute, and ~/.config (RAe).
func configDirs(home string, getenv func(string) string) []string {
	var dirs []string
	if d, ok := absEnv(getenv, envXDGConfig); ok {
		dirs = append(dirs, d)
	}
	return append(dirs, filepath.Join(home, ".config"))
}

// root is a directory and the segments below it where an entry starts.
type root struct {
	dir    string
	prefix []string
}

// roots lists where the files of e live (Vht).
func roots(e entry, home string, getenv func(string) string) []root {
	if e.envDir != "" {
		if d, ok := absEnv(getenv, e.envDir); ok {
			return []root{{dir: d}}
		}
		return nil
	}
	var rs []root
	switch e.base {
	case baseHome:
		rs = append(rs, root{dir: home})
	case baseConfig:
		for _, d := range configDirs(home, getenv) {
			rs = append(rs, root{dir: d})
		}
	case baseCursorData:
		for _, p := range cursorData {
			rs = append(rs, root{dir: home, prefix: []string{p}})
		}
	case baseDaemon:
		for _, p := range daemonDirs {
			rs = append(rs, root{dir: home, prefix: p}, root{dir: home, prefix: append(append([]string{}, p...), daemonBackends...)})
		}
	case baseMoltenData:
		for _, p := range moltenHome {
			rs = append(rs, root{dir: home, prefix: p})
		}
		if d, ok := absEnv(getenv, envMoltenGlass); ok {
			rs = append(rs, root{dir: d})
		}
		for _, d := range configDirs(home, getenv) {
			for _, p := range moltenConfig {
				rs = append(rs, root{dir: d, prefix: p})
			}
		}
	}
	return rs
}

// pattern is a credential file: a directory and the segments below it.
type pattern struct {
	dir      string
	segments []string
}

// table holds the patterns, and their segment matchers by directory (Yht).
type table struct {
	patterns []pattern
	byDir    map[string][][]func(string) bool
	rootsKey string
}

func newTable(home string, getenv func(string) string) *table {
	t := &table{byDir: map[string][][]func(string) bool{}}
	if home == "" || !filepath.IsAbs(home) {
		return t
	}
	seen := map[string]bool{}
	for _, e := range entries {
		segs := strings.Split(e.path, "/")
		dirs, name := segs[:len(segs)-1], segs[len(segs)-1]
		for _, r := range roots(e, home, getenv) {
			// A file also counts with a suffix, or hidden with one (Wht).
			for _, f := range []string{name, name + ".*", "." + name + ".*"} {
				p := append(append(append([]string{}, r.prefix...), dirs...), f)
				dir := filepath.Clean(r.dir)
				key := dir + "\x00" + strings.Join(p, "\x00")
				if seen[key] {
					continue
				}
				seen[key] = true
				t.patterns = append(t.patterns, pattern{dir: dir, segments: p})
				matchers := make([]func(string) bool, len(p))
				for i, seg := range p {
					matchers[i] = glob(seg)
				}
				t.byDir[dir] = append(t.byDir[dir], matchers)
			}
		}
	}
	dirs := make([]string, len(t.patterns))
	for i, p := range t.patterns {
		dirs[i] = p.dir
	}
	t.rootsKey = strings.Join(dirs, "\n")
	return t
}

// glob matches one path segment: * stands for any run of characters (jht).
func glob(seg string) func(string) bool {
	if !strings.Contains(seg, "*") {
		return func(s string) bool { return s == seg }
	}
	parts := strings.Split(seg, "*")
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	re := regexp.MustCompile(`(?s)^` + strings.Join(parts, ".*") + `$`)
	return re.MatchString
}

// match reports whether an absolute path matches a pattern (j3).
func (t *table) match(path string) bool {
	if len(t.byDir) == 0 || !filepath.IsAbs(path) {
		return false
	}
	p := filepath.Clean(path)
	for dir, list := range t.byDir {
		prefix := dir
		if !strings.HasSuffix(prefix, string(filepath.Separator)) {
			prefix += string(filepath.Separator)
		}
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		segs := segments(p[len(prefix):])
		for _, matchers := range list {
			if len(matchers) == len(segs) && all(matchers, segs) {
				return true
			}
		}
	}
	return false
}

func all(matchers []func(string) bool, segs []string) bool {
	for i, m := range matchers {
		if !m(segs[i]) {
			return false
		}
	}
	return true
}

func segments(p string) []string {
	var out []string
	for _, s := range strings.Split(p, string(filepath.Separator)) {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// link maps a directory of a pattern to where it really is.
type link struct{ named, real string }

// links are the symlink mappings of a table's directories (jAt).
type links struct {
	key   string
	built time.Time
	list  []link
}

// buildLinks walks the directories of every pattern as the desktop does:
// the part before the first glob segment is resolved, and a glob segment
// is expanded to the entries it matches that lead further, or are symlinks.
func buildLinks(t *table, now time.Time) *links {
	b := linkBuilder{seenDir: map[string]bool{}, seenWalk: map[string]bool{}, listings: map[string][]os.DirEntry{}}
	for _, p := range t.patterns {
		b.walk(p.dir, p.segments[:len(p.segments)-1])
	}
	return &links{key: t.rootsKey, built: now, list: b.list}
}

type linkBuilder struct {
	seenDir  map[string]bool
	seenWalk map[string]bool
	listings map[string][]os.DirEntry
	list     []link
}

func (b *linkBuilder) walk(folder string, segs []string) {
	key := folder + "\x00" + strings.Join(segs, "\x00")
	if b.seenWalk[key] {
		return
	}
	b.seenWalk[key] = true
	g := -1
	for i, s := range segs {
		if strings.Contains(s, "*") {
			g = i
			break
		}
	}
	fixed := segs
	if g >= 0 {
		fixed = segs[:g]
	}
	dir := filepath.Join(append([]string{folder}, fixed...)...)
	b.resolve(dir)
	if g < 0 {
		return
	}
	match, rest := glob(segs[g]), segs[g+1:]
	list, ok := b.listings[dir]
	if !ok {
		var err error
		if list, err = os.ReadDir(dir); err != nil {
			list = nil // an unreadable directory lists nothing
		}
		b.listings[dir] = list
	}
	for _, e := range list {
		if match(e.Name()) && (len(rest) > 0 || e.Type()&fs.ModeSymlink != 0) {
			b.walk(filepath.Join(dir, e.Name()), rest)
		}
	}
}

func (b *linkBuilder) resolve(dir string) {
	if b.seenDir[dir] {
		return
	}
	b.seenDir[dir] = true
	if resolved, ok := realpath(dir); ok && resolved != dir {
		b.list = append(b.list, link{named: dir, real: resolved})
	}
}

// match reports whether path matches the table directly, or through a
// directory it lies in by its real location (YAt, KAt).
func (l *links) match(t *table, path string) bool {
	if t.match(path) {
		return true
	}
	for _, k := range l.list {
		if named, ok := within(path, k); ok && t.match(named) {
			return true
		}
	}
	return false
}

// within maps a path below k.real to the same place below k.named (KAt, R2).
func within(path string, k link) (string, bool) {
	if k.real == "" {
		return "", false
	}
	p, r := filepath.Clean(path), filepath.Clean(k.real)
	prefix := r
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if p != r && !strings.HasPrefix(p, prefix) {
		return "", false
	}
	rest := segments(p)[len(segments(r)):]
	return filepath.Join(append([]string{k.named}, rest...)...), true
}

// realpath resolves symlinks in an absolute path like the desktop's vo: the
// longest existing ancestor is resolved and the rest kept; any failure other
// than a missing file gives up.
func realpath(path string) (string, bool) {
	t := filepath.Clean(path)
	for n := t; ; {
		s, err := filepath.EvalSymlinks(n)
		if err == nil {
			rel, err := filepath.Rel(n, t)
			if err != nil {
				return "", false
			}
			return filepath.Join(s, rel), true
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", false
		}
		parent := filepath.Dir(n)
		if parent == n {
			return t, true
		}
		n = parent
	}
}
