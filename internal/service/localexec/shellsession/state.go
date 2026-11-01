package shellsession

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/proc"
)

// volatileEnv is set by the shell itself and must not be carried over: bash
// increments an inherited SHLVL, so a saved one would grow with every command.
// environ passes the CLI's SHLVL instead; without one, bash -c ignores
// $BASH_ENV.
var volatileEnv = []string{"SHLVL", "_", "PWD"}

// scopedEnv are the patterns of the desktop's dump (anchor:
// "CURSOR_AGENT_STORE'"), matched without case anywhere in a variable and its
// value: proxy settings and variables of a single request or session are not
// carried over.
var scopedEnv = []string{"_proxy=", "cursor_sandbox", "sudo_askpass", "cursor_askpass",
	"cursor_conversation_id", "cursor_request_id", "cursor_agent_store"}

// scriptVarPrefix starts the variables of the scripts that run commands; "set
// -a" exports them, but they are never state.
const scriptVarPrefix = "__grok_"

// volatileOptions are not carried over. errexit, nounset and pipefail would
// change how every later command and the state dump behave, and xtrace and
// verbose would trace the replay; the others are set by the interactive shell
// that reads the profile, and job control (monitor) would move background jobs
// out of the command's process group. Alias expansion stays off.
var volatileOptions = map[string]bool{
	"errexit": true, "errreturn": true, "unset": true, "nounset": true, "pipefail": true,
	"xtrace": true, "verbose": true, "aliases": true, "expandaliases": true,
	"monitor": true, "interactive": true, "shinstdin": true, "zle": true, "login": true,
	"privileged": true, "restricted": true, "singlecommand": true, "onecmd": true, "exec": true,
	"history": true, "histexpand": true, "emacs": true, "vi": true,
	"login_shell": true, "restricted_shell": true,
}

// State is carried over between foreground commands.
type State struct {
	cwd       string
	env       []string // nil means the CLI's own environment
	options   []string // "set -o x" / "shopt -s x" lines
	functions string
}

// Cwd returns the working directory the state was saved in.
func (st State) Cwd() string { return st.cwd }

// replayScript returns the shell code that restores st. Functions come first,
// so that options such as posix cannot break their definitions; option lines
// that cannot be changed in a non-interactive shell fail quietly.
func (st State) replayScript() string {
	var b strings.Builder
	b.WriteString(st.functions)
	b.WriteString("\n")
	for _, option := range st.options {
		b.WriteString(option + " 2>/dev/null\n")
	}
	return b.String()
}

// environ returns the saved environment with the CLI's SHLVL, and the agent
// markers and extra, the variables of a request, set once.
func (st State) environ(extra []string) []string {
	env := st.env
	if env == nil {
		env = append(os.Environ(), plainEnv...) // later entries win
	} else if level, ok := os.LookupEnv("SHLVL"); ok {
		env = append(env[:len(env):len(env)], "SHLVL="+level)
	}
	set := append(agentEnv[:len(agentEnv):len(agentEnv)], extra...)
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		if !hasKey(set, kv) {
			out = append(out, kv)
		}
	}
	return append(out, set...)
}

// hasKey reports whether one of vars sets the variable of kv.
func hasKey(vars []string, kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	for _, v := range vars {
		if name, _, _ := strings.Cut(v, "="); name == key {
			return true
		}
	}
	return false
}

func readState(f *os.File) (State, bool) {
	b, err := readSnapshot(f)
	if err != nil {
		log.Printf("read shell state: %v", err)
		return State{}, false
	}
	return parseSnapshot(b)
}

func readSnapshot(f *os.File) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSnapshot {
		return nil, fmt.Errorf("shell state of %d bytes exceeds %d", info.Size(), maxSnapshot)
	}
	return proc.ReadAtMost(f, info.Size(), 0)
}

// parseSnapshot decodes the state written by shell.dump. A snapshot without
// the end tag was cut short and is rejected. A snapshot whose environment was
// not written in full has a nil env.
func parseSnapshot(b []byte) (State, bool) {
	b, complete := bytes.CutSuffix(b, []byte("\x00"+sectionEnd+"\x00"))
	if !complete {
		return State{}, false
	}
	parts := strings.Split(string(b), "\x00")
	if len(parts) < 2 || !filepath.IsAbs(parts[0]) {
		return State{}, false
	}
	st := State{cwd: parts[0], env: []string{}}
	sections := map[string]string{}
	envSaved := false
	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if part == sectionEnvSaved && len(sections) == 0 {
			envSaved = true
			continue
		}
		if strings.HasPrefix(part, sectionPrefix) && !strings.Contains(part, "=") {
			if i+1 < len(parts) {
				sections[part] = parts[i+1]
				i++
			}
			continue
		}
		if !envSaved && len(sections) == 0 && strings.Contains(part, "=") && !isVolatileVar(part) {
			st.env = append(st.env, part)
		}
	}
	if !envSaved {
		st.env = nil
	}
	st.options = parseOptions(sections[sectionOptions])
	st.functions = sections[sectionFunctions]
	return st, true
}

func isVolatileVar(kv string) bool {
	key, _, _ := strings.Cut(kv, "=")
	for _, name := range volatileEnv {
		if name == key {
			return true
		}
	}
	if strings.HasPrefix(key, scriptVarPrefix) {
		return true
	}
	lower := strings.ToLower(kv)
	for _, pattern := range scopedEnv {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	return false
}

// parseOptions keeps only well-formed "set ±o name" and "shopt -s/-u name"
// lines, since they are evaluated by the next command.
func parseOptions(text string) []string {
	var options []string
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || !isWord(fields[2]) || isVolatileOption(fields[2]) {
			continue
		}
		set := fields[0] == "set" && (fields[1] == "-o" || fields[1] == "+o")
		shopt := fields[0] == "shopt" && (fields[1] == "-s" || fields[1] == "-u")
		if set || shopt {
			options = append(options, strings.Join(fields, " "))
		}
	}
	return options
}

// isVolatileOption matches option names the way zsh does: case, underscores
// and a "no" prefix do not matter. bash's names are a subset.
func isVolatileOption(name string) bool {
	if volatileOptions[name] {
		return true
	}
	name = strings.ToLower(strings.ReplaceAll(name, "_", ""))
	return volatileOptions[name] || volatileOptions[strings.TrimPrefix(name, "no")]
}

func isWord(s string) bool {
	for _, r := range s {
		if r != '_' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
