package shellsession

import (
	"os"
	"os/exec"
	"strings"
)

// shell is the shell that runs commands, bash or zsh.
type shell struct {
	path string
	zsh  bool
}

// userShell picks the shell like the desktop's kpe: zsh if $SHELL mentions
// zsh, else bash if it mentions bash, each run as $SHELL; else zsh, else bash
// from PATH. PowerShell, which the desktop would pick for a $SHELL mentioning
// it, is not supported.
func userShell() shell {
	name := os.Getenv("SHELL")
	switch {
	case strings.Contains(name, "zsh"):
		return shell{path: name, zsh: true}
	case strings.Contains(name, "bash"):
		return shell{path: name}
	}
	if path, err := exec.LookPath("zsh"); err == nil {
		return shell{path: path, zsh: true}
	}
	if path, err := exec.LookPath("bash"); err == nil {
		return shell{path: path}
	}
	return shell{path: "/bin/bash"}
}

// args runs script with the glob extensions the desktop enables; name is $0
// and params are the positional parameters.
func (s shell) args(flags, script, name string, params ...string) []string {
	args := make([]string, 0, 5+len(params))
	if s.zsh {
		args = append(args, "-o", "extendedglob")
	} else {
		args = append(args, "-O", "extglob")
	}
	return append(append(args, flags, script, name), params...)
}

// dump writes the state as NUL-separated cwd and environment, a tag that the
// environment was written in full, then tagged option, function and alias
// sections and an end tag. Command output cannot be mistaken for state, and state is
// replayed from a file, not parsed back from output.
//
// Like the desktop's dump, the environment leaves out ELECTRON_RUN_AS_NODE
// and, for bash, POSIXLY_CORRECT, which would start the next shell in POSIX
// mode before the functions are replayed; parseSnapshot drops the rest.
func (s shell) dump() string {
	options := `builtin set +o; builtin shopt -p`
	env := `/usr/bin/env -u ELECTRON_RUN_AS_NODE -u POSIXLY_CORRECT -0`
	aliases := `builtin alias -p`
	if s.zsh {
		options = `builtin set +o`
		env = `/usr/bin/env -u ELECTRON_RUN_AS_NODE -0`
		aliases = `builtin alias -L; builtin alias -L -s`
	}
	return strings.Join([]string{
		`builtin printf '%s\0' "$PWD"`,
		env + ` && builtin printf '\0` + sectionEnvSaved + `\0'`,
		`builtin printf '\0` + sectionOptions + `\0'`,
		options,
		`builtin printf '\0` + sectionFunctions + `\0'`,
		`builtin typeset -f`,
		`builtin printf '\0` + sectionAliases + `\0'`,
		aliases,
		`builtin printf '\0` + sectionEnd + `\0'`,
	}, "; ")
}

// initScript dumps the state of the shell that read the profile.
func (s shell) initScript() string {
	return `{ ` + s.dump() + `; } >&4 2>/dev/null`
}

// readCommand reads the command, ended by a NUL, from descriptor 5 into
// $__grok_cmd like the desktop's B3, with its text and exit code if the
// descriptor was closed or replaced by a startup file.
const readCommand = `IFS= builtin read -r -d '' __grok_cmd <&5 2>/dev/null || { builtin printf '%s\n' ` +
	`'The command did not run: a shell startup file ($BASH_ENV or .zshenv) closed or replaced file descriptor 5, ` +
	`which carries the command. Run it again; this shell now passes commands as arguments.' >&2; builtin exit 126; }; `

// prepare reads the command from descriptor 5 into $__grok_cmd if piped and
// the replay script from descriptor 3, closes both and replays the state. The
// command runs from a variable rather than $1, so that it sees no positional
// parameters and eval reports its line numbers unchanged. Like the desktop's,
// a replay script that cannot be read, because a startup file closed the
// descriptor, is skipped. The descriptors are closed with a bare exec before
// the replay, which could define a function named exec; "builtin exec" would
// only close them for the duration of the builtin.
//
// The aliases of a startup file ($BASH_ENV, .zshenv) are removed and the
// replay runs without alias expansion, so that no alias changes how the
// replayed functions parse. The command then runs with the session's aliases
// expanded, as on the desktop (State.aliases), and without Node's module
// compile cache, as on the desktop since 1.0.1 (e1); the state dump keeps
// the variable like any other.
func (s shell) prepare(piped bool) string {
	clearAliases := `builtin shopt -u expand_aliases 2>/dev/null; builtin unalias -a 2>/dev/null; `
	expand := `builtin shopt -s expand_aliases 2>/dev/null; `
	if s.zsh {
		clearAliases = `builtin unsetopt aliases 2>/dev/null; builtin unalias -m '*' 2>/dev/null; ` +
			`builtin unalias -s -m '*' 2>/dev/null; `
		expand = `builtin setopt aliases 2>/dev/null; `
	}
	read := ""
	if piped {
		read = readCommand
	}
	return read + `{ __grok_replay=$(</dev/fd/3); } 2>/dev/null || __grok_replay=; ` +
		`exec 3<&- 5<&-; ` +
		clearAliases + `builtin eval -- "$__grok_replay"; builtin unset __grok_replay; ` +
		noCompileCache + expand
}

// noCompileCache turns off Node's module compile cache for the command.
const noCompileCache = `builtin export NODE_DISABLE_COMPILE_CACHE=1; builtin unset NODE_COMPILE_CACHE; `

// aliasesOff starts the EXIT trap. The trap is parsed when it runs, with the
// aliases the command defined; the quoted words are not expanded, and what
// they evaluate is parsed with expansion off.
func (s shell) aliasesOff() string {
	if s.zsh {
		return `\builtin \unsetopt \aliases 2>/dev/null; `
	}
	return `\builtin \shopt -u expand_aliases 2>/dev/null; `
}

// foregroundScript runs the command with descriptor 4 closed and then dumps
// the state to it. $1 names the fallback snapshot file for the EXIT trap and,
// unless piped, $2 is the command. The script's own variables start unset, so
// that an inherited __grok_dumped cannot disable the trap; under "set -a" they
// are exported to the dump, which parseSnapshot drops. The trap overwrites
// the fallback file even under noclobber. The script is one line, which the
// shell parses before it runs any of it: aliases of the command do not reach
// the dump after it.
func (s shell) foregroundScript(piped bool) string {
	subshell := "BASH_SUBSHELL"
	if s.zsh {
		subshell = "ZSH_SUBSHELL"
	}
	dump := `{ builtin set +xveu; builtin test -z "$__grok_dumped" && builtin test "$` + subshell + `" = 0 && ` +
		`{ ` + s.dump() + `; } >|"$__grok_fallback"; } 2>/dev/null`
	trap := s.aliasesOff() + `\builtin \eval ` + shellQuote(dump)
	command := ""
	if !piped {
		command = `__grok_cmd=$2; `
	}
	return `builtin unset __grok_dumped __grok_exit; builtin readonly __grok_fallback="$1"; ` + command + `builtin set --; ` +
		s.prepare(piped) +
		`builtin trap ` + shellQuote(trap) + ` EXIT; ` +
		`builtin eval -- "$__grok_cmd" 4>&-; ` +
		`{ __grok_exit=$?; __grok_dumped=1; builtin set +xv; builtin unset __grok_cmd; ` + s.dump() + `; } >&4 2>/dev/null; ` +
		`builtin exit "$__grok_exit"`
}

// backgroundScript waits for a line on descriptor 6 before running the
// command, so the terminal header is written before any output, and then runs
// it like foregroundScript, which saves the state it leaves.
func (s shell) backgroundScript(piped bool) string {
	return `IFS= builtin read -r __grok_start <&6; exec 6<&-; builtin unset __grok_start; ` + s.foregroundScript(piped)
}

// shellQuote quotes s as a single word for bash and zsh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
