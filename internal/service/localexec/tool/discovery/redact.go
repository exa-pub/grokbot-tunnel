package discovery

import (
	"regexp"
	"strings"
	"unicode/utf16"
)

// secretPatterns match API keys and tokens of common providers, bearer
// tokens, basic credentials, JWTs and long hex strings: the desktop's (Pqt)
// and a few more.
var secretPatterns = []string{
	`\b(?:sk|pk|rk)-[A-Za-z0-9_-]{16,}\b`,
	`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{10,}\b`,
	`\bglpat-[A-Za-z0-9_-]{20,}\b`,
	`\bhf_[A-Za-z0-9]{30,}\b`,
	`\bnpm_[A-Za-z0-9]{36}\b`,
	`(?i:Authorization:\s*Basic\s+[A-Za-z0-9+/=]{8,})`,
	`\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{16,}\b`,
	`\bxox[abpors]-[A-Za-z0-9-]{10,}\b`,
	`\bAKIA[0-9A-Z]{16}\b`,
	`\bAIza[0-9A-Za-z_-]{30,}\b`,
	`(?i:Bearer\s+[A-Za-z0-9._~+/=-]{16,})`,
	`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`,
	`\b[A-Fa-f0-9]{32,}\b`,
}

// redactors strip secrets, links and emails from discovered titles, in the
// desktop's order (Bp).
var redactors = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{regexp.MustCompile(strings.Join(secretPatterns, "|")), "[redacted]"},
	{regexp.MustCompile(`([=:]\s*["']?)[A-Za-z0-9+/_-]{32,}={0,2}`), "${1}[redacted]"},
	// AWS secret access keys: 40 characters of base64, often with slashes.
	{regexp.MustCompile(`(^|[\s"'])[A-Za-z0-9+][A-Za-z0-9+/]{39}($|[\s"',;.])`), "${1}[redacted]${2}"},
	{regexp.MustCompile("(?i)\\b[a-z][a-z0-9+.-]*://[^\\s<>\"'`]+"), "[link]"},
	{regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`), "[email]"},
	{regexp.MustCompile("\\b[A-Za-z]:\\\\(?:[^\\s\\\\<>\"'`,;)]+\\\\?)+"), "[path]"}, // Mqt
}

// unixPath is the desktop's path pattern (Nqt) without its lookbehind
// (?<![\w.:-]), which redactPaths checks.
var unixPath = regexp.MustCompile("^(?:~|\\.{1,2})?(?:/[^\\s/<>\"'`,;)]+){2,}/?")

var (
	slashCommand = regexp.MustCompile(`^/[A-Za-z][\w:-]*`)
	pastedText   = regexp.MustCompile(`\[(?:Pasted text|Image) #\d+[^\]]*\]`)
	mcpSection   = regexp.MustCompile(`^\s*\[\s*mcp_servers\.(?:"([^"]+)"|([A-Za-z0-9_-]+))\s*(\.[^\]]*)?\]\s*$`)
	mcpTable     = regexp.MustCompile(`^\s*\[`)
	mcpCommand   = regexp.MustCompile(`^\s*command\s*=\s*"([^"]*)"`)
	mcpURL       = regexp.MustCompile(`^\s*url\s*=\s*"`)
	lineBreak    = regexp.MustCompile(`\r?\n`)
)

// jsSpace is a character that JavaScript's \s and trim match.
func jsSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xa0, 0x1680, 0x2028, 0x2029, 0x202f, 0x205f, 0x3000, 0xfeff:
		return true
	}
	return r >= 0x2000 && r <= 0x200a
}

func jsTrim(s string) string { return strings.TrimFunc(s, jsSpace) }

// collapseSpace replaces runs of whitespace with one space and trims.
func collapseSpace(s string) string {
	var b strings.Builder
	space := false
	for _, r := range s {
		if jsSpace(r) {
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return b.String()
}

// redact normalizes whitespace and replaces secrets, links, emails and
// paths, as the desktop's Bp does before shortening.
func redact(text string) string {
	text = collapseSpace(text)
	for _, r := range redactors {
		text = r.pattern.ReplaceAllString(text, r.replacement)
	}
	return redactPaths(text)
}

// redactPaths replaces paths of two or more components that do not follow a
// word character, dot, colon or hyphen, so that src/main/java stays.
func redactPaths(s string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); i++ {
		if c := s[i]; c != '/' && c != '.' && c != '~' || i > 0 && pathNeighbor(s[i-1]) {
			continue
		}
		if loc := unixPath.FindStringIndex(s[i:]); loc != nil {
			b.WriteString(s[last:i])
			b.WriteString("[path]")
			last = i + loc[1]
			i = last - 1
		}
	}
	if last == 0 {
		return s
	}
	return b.String() + s[last:]
}

func pathNeighbor(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == ':' || c == '-'
}

// discoveryTitle redacts and shortens text (Bp).
func discoveryTitle(text string) string { return shorten(redact(text)) }

// commandTitle names a command by its redacted namespace and name (eFe).
func commandTitle(parts []string) string {
	redacted := make([]string, len(parts))
	for i, part := range parts {
		redacted[i] = redact(part)
	}
	return shorten(strings.Join(redacted, ":"))
}

// shorten cuts text longer than maxTitleUnits UTF-16 code units to one less,
// trims trailing whitespace and appends an ellipsis (tFe). A surrogate pair
// is not split.
func shorten(text string) string {
	units := utf16.Encode([]rune(text))
	if len(units) <= maxTitleUnits {
		return text
	}
	n := maxTitleUnits - 1
	if utf16.IsSurrogate(rune(units[n-1])) && units[n-1] < 0xdc00 {
		n-- // do not split a surrogate pair
	}
	return strings.TrimRightFunc(string(utf16.Decode(units[:n])), jsSpace) + "…"
}

// nodeBase is Node's path.basename.
func nodeBase(path string) string {
	path = strings.TrimRight(path, "/")
	return path[strings.LastIndexByte(path, '/')+1:]
}

// commandExcerpt describes an MCP server started by command (LFe).
func commandExcerpt(command string) string { return discoveryTitle("stdio · " + nodeBase(command)) }

// dedupItems drops untitled and repeated items, keeping the first (PFe).
func dedupItems(items []discoveryItem) []discoveryItem {
	seen := map[string]bool{}
	out := []discoveryItem{}
	for _, item := range items {
		key := item.Kind + "\n" + item.Title
		if item.Title != "" && !seen[key] {
			seen[key] = true
			out = append(out, item)
		}
	}
	return out
}

// codexMCP lists the MCP servers in a Codex config.toml with their transport
// (xGt). It understands just enough TOML for [mcp_servers.<name>] tables.
func codexMCP(text string) []discoveryItem {
	var order []string
	transport := map[string]string{}
	current := ""
	for _, line := range lineBreak.Split(text, -1) {
		if match := mcpSection.FindStringSubmatch(line); match != nil {
			name := match[1] + match[2]
			if _, ok := transport[name]; !ok {
				order = append(order, name)
				transport[name] = "unknown"
			}
			current = name
			if match[3] != "" {
				current = "" // a subtable such as [mcp_servers.x.env]
			}
			continue
		}
		if mcpTable.MatchString(line) {
			current = ""
			continue
		}
		if current == "" {
			continue
		}
		if match := mcpCommand.FindStringSubmatch(line); match != nil {
			transport[current] = commandExcerpt(match[1])
		} else if mcpURL.MatchString(line) {
			transport[current] = "http"
		}
	}
	items := make([]discoveryItem, 0, len(order))
	for _, name := range order {
		items = append(items, discoveryItem{Kind: "mcp_server", Title: discoveryTitle(name), Excerpt: transport[name]})
	}
	return items
}
