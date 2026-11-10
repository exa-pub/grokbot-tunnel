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
// $BASH_ENV. bash keeps SHELLOPTS and BASHOPTS in step with its options and
// takes exported ones as the options to start with, so a saved one would set
// volatile options such as nounset and pipefail in every later shell; the
// desktop's replay of these read-only variables fails.
var volatileEnv = []string{"SHLVL", "_", "PWD", "SHELLOPTS", "BASHOPTS"}

// replayedIFS is restored by the replay: bash and zsh do not take IFS from
// the environment, and the desktop's replay of "export -p" sets an exported
// one.
const replayedIFS = "IFS"

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
// out of the command's process group. Alias expansion is turned on for every
// command (shell.prepare), whatever the last one left.
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
	// aliases are the alias definitions of the commands so far, as the shell
	// prints them; those of the user's profile are left out (Session.Init).
	aliases string
}

// Cwd returns the working directory the state was saved in.
func (st State) Cwd() string { return st.cwd }

// startupEnv returns the variables that sh reads when it starts, before the
// replay: bash runs $BASH_ENV, and $ENV in POSIX mode; zsh reads its startup
// files from $ZDOTDIR, else $HOME, and runs $ENV when emulating sh. The
// desktop starts every shell with the app's environment and replays the saved
// one inside it (rVt; anchor: "builtin export -p"), so a command that exports
// one of them changes its children, not the shells of later commands. Like
// there, a shell starts with the CLI's values of these (environ), and the
// replay exports the saved ones (replayEnv).
func (s shell) startupEnv() []string {
	if s.zsh {
		return []string{"ZDOTDIR", "HOME", "ENV"}
	}
	return []string{"BASH_ENV", "ENV"}
}

// replayScript returns the shell code that restores st for sh. The saved
// values of the variables the shell reads at startup come first, then
// aliases, before a replayed function could be named alias, then functions,
// so that options such as posix cannot break their definitions; option lines
// that cannot be changed in a non-interactive shell fail quietly. The shell
// evaluates it with alias expansion off (shell.prepare).
func (st State) replayScript(sh shell) string {
	var b strings.Builder
	b.WriteString(st.replayEnv(sh))
	b.WriteString(st.aliases)
	b.WriteString("\n")
	b.WriteString(st.functions)
	b.WriteString("\n")
	for _, option := range st.options {
		b.WriteString(option + " 2>/dev/null\n")
	}
	return b.String()
}

// replayEnv returns the code that exports, or unsets, the saved values of
// the variables of sh's startupEnv that differ from the CLI's, which environ
// starts the shell with, and exports a saved IFS. Builtins are called as
// such, ahead of the replayed functions.
func (st State) replayEnv(sh shell) string {
	if st.env == nil {
		return "" // the CLI's environment, which the shell starts with
	}
	var b strings.Builder
	export := func(key, value string) {
		b.WriteString("builtin export " + key + "=" + shellQuote(value) + "\n")
	}
	for _, key := range sh.startupEnv() {
		value, saved := lookupEnv(st.env, key)
		cli, ok := os.LookupEnv(key)
		switch {
		case saved && (!ok || value != cli):
			export(key, value)
		case !saved && ok:
			b.WriteString("builtin unset " + key + " 2>/dev/null\n")
		}
	}
	if value, ok := lookupEnv(st.env, replayedIFS); ok {
		export(replayedIFS, value)
	}
	return b.String()
}

// environ returns the environment that sh starts with: the saved one with
// the CLI's SHLVL and the CLI's values of the variables of sh's startupEnv,
// and the agent markers and extra, the variables of a request, set once.
func (st State) environ(sh shell, extra []string) []string {
	env := st.env
	var set, startup []string
	if env == nil {
		env = append(os.Environ(), plainEnv...) // later entries win
	} else {
		startup = sh.startupEnv()
		for _, key := range append([]string{"SHLVL"}, startup...) {
			if value, ok := os.LookupEnv(key); ok {
				set = append(set, key+"="+value)
			}
		}
	}
	set = append(append(set, agentEnv...), extra...)
	out := make([]string, 0, len(env)+len(set))
	for _, kv := range env {
		if !hasKey(set, kv) && !hasKey(startup, kv) {
			out = append(out, kv)
		}
	}
	return append(out, set...)
}

// lookupEnv returns the value of key in env, the last one if set twice.
func lookupEnv(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if name, value, ok := strings.Cut(env[i], "="); ok && name == key {
			return value, true
		}
	}
	return "", false
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
	st.aliases = sections[sectionAliases]
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
