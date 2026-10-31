package discovery

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
)

// secretPatterns match API keys and tokens of common providers, bearer
// tokens, basic credentials, JWTs and long hex strings.
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

// redactors strip secrets, links, emails and paths from discovered titles.
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
	{regexp.MustCompile("[A-Za-z]:\\\\[^\\s<>\"'`,;]+|(?:~|\\.{1,2})?(?:/[^\\s/<>\"'`,;)]+){2,}/?"), "[path]"},
}

var (
	slashCommand = regexp.MustCompile(`^/[A-Za-z][\w:-]*(?:\s|$)`)
	pastedText   = regexp.MustCompile(`\[(?:Pasted text|Image) #\d+[^\]]*\]`)
	mcpSection   = regexp.MustCompile(`^\s*\[\s*mcp_servers\.(?:"([^"]+)"|([A-Za-z0-9_-]+))\s*(\.[^\]]*)?\]\s*$`)
	mcpCommand   = regexp.MustCompile(`^\s*command\s*=\s*"([^"]*)"`)
	mcpURL       = regexp.MustCompile(`^\s*url\s*=\s*"`)
)

// discoveryTitle normalizes whitespace, redacts and shortens text.
func discoveryTitle(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	for _, r := range redactors {
		text = r.pattern.ReplaceAllString(text, r.replacement)
	}
	if units := utf16.Encode([]rune(text)); len(units) > maxTitleUnits {
		n := maxTitleUnits - 1
		if utf16.IsSurrogate(rune(units[n-1])) && units[n-1] < 0xdc00 {
			n-- // do not split a surrogate pair
		}
		text = string(utf16.Decode(units[:n])) + "…"
	}
	return text
}

// The discovery paths come from the user's own home directory and
// environment, outside Root by design.

// dedupItems drops untitled and repeated items, keeping the first.
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

// codexMCP lists the MCP servers in a Codex config.toml with their transport.
// It understands just enough TOML for [mcp_servers.<name>] tables.
func codexMCP(text string) []discoveryItem {
	var order []string
	transport := map[string]string{}
	current := ""
	for _, line := range strings.Split(text, "\n") {
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
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			current = ""
		}
		if current == "" {
			continue
		}
		if match := mcpCommand.FindStringSubmatch(line); match != nil {
			transport[current] = "stdio · " + filepath.Base(match[1])
		} else if mcpURL.MatchString(line) {
			transport[current] = "http"
		}
	}
	items := make([]discoveryItem, 0, len(order))
	for _, name := range order {
		items = append(items, discoveryItem{Kind: "mcp_server", Title: discoveryTitle(name), Excerpt: discoveryTitle(transport[name])})
	}
	return items
}
