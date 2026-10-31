package shellsession

import (
	"fmt"
	"strconv"
	"strings"
)

// agentEnv marks processes started on behalf of the agent.
var agentEnv = []string{"CURSOR_AGENT=1", "SAND_AGENT=1"}

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
