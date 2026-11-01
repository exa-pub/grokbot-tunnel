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
// environment was written in full, then tagged option and function sections
// and an end tag. Command output cannot be mistaken for state, and state is
// replayed from a file, not parsed back from output.
//
// Like the desktop's dump, the environment leaves out ELECTRON_RUN_AS_NODE
// and, for bash, POSIXLY_CORRECT, which would start the next shell in POSIX
// mode before the functions are replayed; parseSnapshot drops the rest.
func (s shell) dump() string {
	options := `builtin set +o; builtin shopt -p`
	env := `/usr/bin/env -u ELECTRON_RUN_AS_NODE -u POSIXLY_CORRECT -0`
	if s.zsh {
		options = `builtin set +o`
		env = `/usr/bin/env -u ELECTRON_RUN_AS_NODE -0`
	}
	return strings.Join([]string{
		`builtin printf '%s\0' "$PWD"`,
		env + ` && builtin printf '\0` + sectionEnvSaved + `\0'`,
		`builtin printf '\0` + sectionOptions + `\0'`,
		options,
		`builtin printf '\0` + sectionFunctions + `\0'`,
		`builtin typeset -f`,
		`builtin printf '\0` + sectionEnd + `\0'`,
	}, "; ")
}

// initScript dumps the state of the shell that read the profile.
func (s shell) initScript() string {
	return `{ ` + s.dump() + `; } >&4 2>/dev/null`
}

// prepare reads the replay script from descriptor 3 and the command from
// descriptor 5 into $__grok_cmd, closes both and replays the state. The
// command runs from a variable rather than $1, so that it sees no positional
// parameters and eval reports its line numbers unchanged. The descriptors are
// closed with a bare exec before the replay, which could define a function
// named exec; "builtin exec" would only close them for the duration of the
// builtin.
func (s shell) prepare() string {
	unalias := `builtin shopt -u expand_aliases 2>/dev/null; `
	if s.zsh {
		// zsh expands aliases even when not interactive; aliases from
		// .zshenv must not change how the replayed functions parse either.
		unalias = `builtin unsetopt aliases 2>/dev/null; builtin unalias -m '*' 2>/dev/null; `
	}
	return `__grok_cmd=$(</dev/fd/5) && __grok_replay=$(</dev/fd/3) || { ` +
		`builtin printf '%s\n' 'The command did not run: a shell startup file ($BASH_ENV or .zshenv) closed descriptor 3 or 5.' >&2; ` +
		`builtin exit 126; }; ` +
		`exec 3<&- 5<&-; ` +
		unalias + `builtin eval -- "$__grok_replay"; builtin unset __grok_replay; `
}

// foregroundScript runs the command with descriptor 4 closed and then dumps
// the state to it. $1 names the fallback snapshot file for the EXIT trap.
// The script's own variables start unset, so that an inherited __grok_dumped
// cannot disable the trap; under "set -a" they are exported to the dump, which
// parseSnapshot drops. The trap overwrites the fallback file even under
// noclobber.
func (s shell) foregroundScript() string {
	subshell := "BASH_SUBSHELL"
	if s.zsh {
		subshell = "ZSH_SUBSHELL"
	}
	trap := `{ builtin set +xveu; builtin test -z "$__grok_dumped" && builtin test "$` + subshell + `" = 0 && ` +
		`{ ` + s.dump() + `; } >|"$__grok_fallback"; } 2>/dev/null`
	return `builtin unset __grok_dumped __grok_exit; builtin readonly __grok_fallback="$1"; builtin set --; ` +
		s.prepare() +
		`builtin trap ` + shellQuote(trap) + ` EXIT; ` +
		`builtin eval -- "$__grok_cmd" 4>&-; ` +
		`{ __grok_exit=$?; __grok_dumped=1; builtin set +xv; builtin unset __grok_cmd; ` + s.dump() + `; } >&4 2>/dev/null; ` +
		`builtin exit "$__grok_exit"`
}

// backgroundScript waits for a line on descriptor 6 before running the
// command, so the terminal header is written before any output, and then runs
// it like foregroundScript, which saves the state it leaves.
func (s shell) backgroundScript() string {
	return `IFS= builtin read -r __grok_start <&6; exec 6<&-; builtin unset __grok_start; ` + s.foregroundScript()
}

// shellQuote quotes s as a single word for bash and zsh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
