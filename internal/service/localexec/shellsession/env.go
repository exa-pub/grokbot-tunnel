package shellsession

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// agentEnv marks processes started on behalf of the agent.
var agentEnv = []string{"CURSOR_AGENT=1", "SAND_AGENT=1"}

// transcriptsDir is the desktop's agent transcripts directory below the
// project, which on local-exec is the root (ioe).
const transcriptsDir = "agent-transcripts"

// evalMarkers make the desktop hash a request ID (GZt).
var evalMarkers = []string{"eval-cursorbench", "cursorbench", "sand-eval", "eval-harness"}

// RequestEnv returns the variables the desktop's shell core executor (D3)
// adds for a command of a request: CURSOR_CONVERSATION_ID and
// CURSOR_REQUEST_ID when given, and AGENT_TRANSCRIPTS below root.
func RequestEnv(root, conversationID, requestID string) []string {
	var env []string
	if conversationID != "" {
		env = append(env, "CURSOR_CONVERSATION_ID="+conversationEnv(conversationID))
	}
	if requestID != "" {
		env = append(env, "CURSOR_REQUEST_ID="+requestEnv(requestID))
	}
	return append(env, "AGENT_TRANSCRIPTS="+filepath.Join(root, transcriptsDir))
}

// conversationEnv encodes a conversation ID like the desktop's aoe:
// encodeURIComponent with "%" as "_", at most 200 characters.
func conversationEnv(id string) string {
	const unreserved = "-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(id); i++ {
		c := id[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "_%02X", c)
		}
	}
	s := b.String()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// requestEnv returns a request ID like the desktop's DVe: the SHA-256 of an
// evaluation run's ID, any other as it is.
func requestEnv(id string) string {
	for _, marker := range evalMarkers {
		if strings.Contains(id, marker) {
			sum := sha256.Sum256([]byte(id))
			return hex.EncodeToString(sum[:])
		}
	}
	return id
}

// plainEnv asks programs for plain output, as the desktop's shells do. It
// joins the CLI's environment; a saved state carries it on, so commands can
// change it.
var plainEnv = []string{"TERM=dumb", "NO_COLOR=1", "FORCE_COLOR=0", "_ZO_DOCTOR=0"}

// gitConfig is git configuration the desktop gives agent commands: git
// prints non-ASCII file names as they are instead of quoting them.
var gitConfig = [][2]string{{"core.quotePath", "false"}}

// withGitConfig adds gitConfig to env as GIT_CONFIG_KEY_n and
// GIT_CONFIG_VALUE_n after the GIT_CONFIG_COUNT entries env already has,
// unless one of them sets the key. Later entries of env win.
func withGitConfig(env []string) []string {
	count := 0
	keys := map[string]string{}
	for _, kv := range env {
		key, value, _ := strings.Cut(kv, "=")
		if key == "GIT_CONFIG_COUNT" {
			count = parseCount(value)
		} else if strings.HasPrefix(key, "GIT_CONFIG_KEY_") {
			keys[key] = value
		}
	}
	set := map[string]bool{}
	for key, value := range keys {
		if i, err := strconv.Atoi(strings.TrimPrefix(key, "GIT_CONFIG_KEY_")); err == nil && i >= 0 && i < count {
			set[strings.ToLower(value)] = true
		}
	}
	n := count
	for _, c := range gitConfig {
		if !set[strings.ToLower(c[0])] {
			env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", n, c[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", n, c[1]))
			n++
		}
	}
	if n != count {
		env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(n))
	}
	return env
}

// parseCount reads GIT_CONFIG_COUNT like JavaScript's parseInt: leading
// digits after optional space and sign; anything else, or a negative
// number, is 0.
func parseCount(s string) int {
	s = strings.TrimPrefix(strings.TrimLeft(s, " \t\n\v\f\r"), "+")
	end := 0
	for end < len(s) && s[end] >= '0' && s[end] <= '9' {
		end++
	}
	n, err := strconv.Atoi(s[:end])
	if err != nil {
		return 0 // no digits, a sign or too large
	}
	return n
}
