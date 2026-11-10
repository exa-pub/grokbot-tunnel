package scenarios

import (
	"regexp"
	"time"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	. "github.com/exa-pub/grokbot-tunnel/tests/compare/scenario" //nolint:revive,staticcheck // scenarios read as a list of steps
	"github.com/exa-pub/grokbot-tunnel/tests/compare/transcript"
)

const zsh = "/bin/zsh"

// timed is a shell command with a timeout in milliseconds.
func timed(id, command string, timeout int32, behavior agentv1.TimeoutBehavior) Frame {
	return ShellArgs(id, &agentv1.ShellArgs{Command: command, Timeout: timeout, TimeoutBehavior: behavior})
}

// session is a scenario of shell commands sent one after the other, each a
// request of its own.
func session(id, ref, about string, commands ...string) Scenario {
	s := Scenario{ID: id, Ref: ref, About: about}
	for i, c := range commands {
		s.Steps = append(s.Steps, Do(Shell("r"+string(rune('1'+i)), c)))
	}
	return s
}

// anyExecID is for scenarios with requests that start at once: which of
// them gets which exec ID depends on which starts first.
var anyExecID = []transcript.Rule{
	{Path: "requests/*/*/client/id", Replace: "<ID>"}, {Path: "requests/*/*/control/*/id", Replace: "<ID>"},
}

// aboutOneMiB compares how much of a large output is streamed only roughly:
// the desktop stops streaming when it falls behind, which is near 1 MiB but
// not at the same byte on every run.
var aboutOneMiB = []transcript.Rule{{
	Path: "requests/*/*/client/shellStream/stdout/data",
	Func: func(v any) any {
		if s, ok := v.(string); ok && len(s) > 900<<10 && len(s) <= 1<<20 {
			return "<about 1 MiB starting with " + s[:16] + ">"
		}
		return v
	},
}}

var shell = []Scenario{
	session("shell/echo", "A-1", "stdout and exit code 0", "echo hello"),
	session("shell/pwd", "A-2", "a new session starts in the root", "pwd"),
	session("shell/agent-env", "A-3", "the variables that tell a program an agent runs it",
		`echo "$CURSOR_AGENT $SAND_AGENT $TERM $NO_COLOR"`),
	{
		ID: "shell/env-names", About: "which variables a command sees",
		Steps: []Step{Do(Shell("r1", `env | cut -d= -f1 | sort`))},
		// What the test gives each client to point it at the fake backend
		// is not what a user's command would see.
		Rules: []transcript.Rule{{
			Path: "requests/r1/*/client/shellStream/stdout/data",
			Pattern: regexp.MustCompile(
				`(?m)^(SAND_(BACKEND_URL|CURSOR_WEBSITE_URL|DATA_ROOT|LOCAL_EXEC_\w+|DISABLE_TELEMETRY)|GROKBOT_COMPARE_RUN)\n`),
		}},
	},
	session("shell/git-quotepath", "A-4", "git is told not to quote paths", `git config --get core.quotePath; echo "rc=$?"`),
	session("shell/exit-code", "A-5", "a non-zero exit code is a result, not an error", "exit 3"),
	{
		ID: "shell/stdout-stderr", Ref: "A-6", About: "both streams and the exit code", OrderedOutput: true,
		Steps: []Step{Do(Shell("r1", "echo out; sleep 0.2; echo err >&2; exit 1"))},
	},
	session("shell/unicode", "A-7", "UTF-8 output", `printf 'Привет, 世界 🙂\n'`),
	session("shell/invalid-utf8", "A-8", "bytes that are not UTF-8", `printf '\xff\xfe broken\n'`),
	session("shell/not-found", "A-9", "a command that does not exist", "command_that_does_not_exist_xyz"),
	session("shell/stdin-empty", "A-10", "stdin is empty, cat ends at once", "cat; echo rc=$?"),
	session("shell/read-stdin", "C-7", "read from the empty stdin", `read -t 1 x; echo "rc=$?"`),
	session("shell/no-output", "", "a command without output", "true"),
	session("shell/no-newline", "", "output without a final newline", "printf abc"),
	session("shell/multiline-command", "", "a command of several lines", "for i in 1 2 3; do\n  echo $i\ndone"),
	session("shell/syntax-error", "", "a command the shell cannot parse", "if then fi ("),
	session("shell/tty", "", "the command has no terminal", `test -t 0; echo $?; test -t 1; echo $?; tty`),

	// The session keeps state between requests.
	session("shell/session-cd", "B-1", "the working directory", "mkdir -p sub && cd sub", "pwd"),
	session("shell/session-export", "B-2", "exported variables stay, others do not",
		"export GROK_T1=value1; LOCAL_T=local", `echo "[$GROK_T1] [$LOCAL_T]"`),
	session("shell/session-function", "B-3", "functions", `gt() { echo "fn:$1"; }`, "gt ok"),
	session("shell/session-alias", "B-4", "aliases", "alias gta='echo alias'", "gta; echo rc=$?"),
	session("shell/session-unalias", "", "an alias is removed", "alias gtu='echo alias'", "unalias gtu", "gtu 2>/dev/null; echo rc=$?"),
	{
		ID: "shell/profile-alias", About: "an alias and a function of the user's profile",
		Home: []File{
			Text(".bash_profile", ". \"$HOME/.bashrc\"\n"),
			Text(".bashrc", "alias gtp='echo profile-alias'\ngtf() { echo profile-function; }\n"),
		},
		Steps: []Step{Do(Shell("r1", "gtf; gtp 2>/dev/null; echo rc=$?"))},
	},
	session("shell/session-shopt", "B-5", "shell options", "shopt -s extglob", "shopt extglob"),
	session("shell/session-nounset", "B-6", "set -u", "set -u", `echo "[$UNDEFINED_T]"; echo rc=$?`),
	session("shell/session-pipefail", "B-7", "set -o pipefail", "set -o pipefail", "false | true; echo $?"),
	session("shell/session-errexit", "B-8", "set -e ends the command, not the session", "set -e; false; echo after", "echo next"),
	session("shell/session-umask", "B-9", "umask", "umask 077", "umask"),
	session("shell/session-exit", "B-10", "the state stays after exit", "cd /tmp", "exit 0", "pwd"),
	session("shell/session-unset", "B-11", "an unset variable stays unset",
		"export GROK_T1=value1", "export -n GROK_T1; unset GROK_T1", `echo "[${GROK_T1-unset}]"`),
	session("shell/session-ifs", "B-12", "an exported IFS", "export IFS=:", `printf '[%s]' "$IFS"`),
	session("shell/session-shellopts", "B-13", "exported SHELLOPTS does not pin options",
		"set -u -o pipefail; export SHELLOPTS", `echo "[$UNDEFINED_T2]"; false | true; echo $?`),
	session("shell/session-bash-env", "B-14", "BASH_ENV is kept and not sourced",
		`printf 'echo SOURCED\n' > benv.sh; export BASH_ENV=$PWD/benv.sh`, "echo cmd", `echo "$BASH_ENV"`),
	session("shell/session-failed-cd", "", "a cd that fails", "cd /nonexistent/dir; echo rc=$?", "pwd"),
	session("shell/session-deleted-cwd", "", "the session's directory is removed",
		"mkdir gone && cd gone", "rmdir ../gone", "pwd; echo rc=$?"),
	session("shell/session-readonly", "", "a read-only variable", "readonly GROK_RO=1", `echo "[$GROK_RO]"`, "echo again"),
	session("shell/session-multiline-var", "", "a variable with newlines and quotes",
		`export GROK_ML=$'line1\nline "2"\n'"'three'"`, `printf '%s|' "$GROK_ML"`),
	session("shell/session-oldpwd", "", "cd - after a request", "mkdir -p a b && cd a", "cd ../b", "cd - && pwd"),

	// Working directory of a request.
	{
		ID: "shell/cwd", About: "a working directory inside the root",
		Files: []File{Dir("work")},
		Steps: []Step{Do(ShellArgs("r1", &agentv1.ShellArgs{Command: "pwd", WorkingDirectory: "work"})), Do(Shell("r2", "pwd"))},
	},
	{
		ID: "shell/cwd-missing", Ref: "E-6", About: "a working directory that does not exist",
		Steps: []Step{Do(ShellArgs("r1", &agentv1.ShellArgs{Command: "pwd", WorkingDirectory: "/nonexistent/x"}))},
	},
	{
		ID: "shell/cwd-file", About: "a working directory that is a file",
		Files: []File{Text("plain", "x")},
		Steps: []Step{Do(ShellArgs("r1", &agentv1.ShellArgs{Command: "pwd", WorkingDirectory: "plain"}))},
	},
	{
		ID: "shell/cwd-outside", About: "a working directory outside the root",
		Steps: []Step{Do(ShellArgs("r1", &agentv1.ShellArgs{Command: "pwd", WorkingDirectory: "/tmp"}))},
	},

	// Output size.
	{
		ID: "shell/output-lines", Ref: "C-1", About: "many lines of output are cut",
		Steps: []Step{Do(Shell("r1", "seq 1 200000"))},
		Rules: aboutOneMiB,
	},
	{
		ID: "shell/output-large", Ref: "C-2", About: "3 MB of output are cut",
		Steps: []Step{Do(Shell("r1", `head -c 3000000 /dev/zero | tr '\0' 'a'; echo`))},
		Watch: []string{"agent-tools"},
		Rules: aboutOneMiB,
	},
	{
		ID: "shell/output-threshold", About: "a low threshold for the output file",
		Steps: []Step{Do(ShellArgs("r1", &agentv1.ShellArgs{Command: "seq 1 2000", FileOutputThresholdBytes: proto.Uint64(1000)}))},
		Watch: []string{"agent-tools"},
	},
	session("shell/output-long-line", "", "one long line", `head -c 200000 /dev/zero | tr '\0' 'x'; echo`),
	session("shell/output-stderr-large", "", "a lot of stderr", `seq 1 50000 >&2`),

	// Timeouts, cancellation and concurrency.
	{
		ID: "shell/timeout-background", Ref: "C-3", About: "a command that outlives its timeout goes to the background",
		Steps: []Step{
			Do(timed("r1", "sleep 4; echo late", 1000, agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND)),
			Do(Shell("r2", "echo ok")),
			Sleep(5 * time.Second),
		},
		Watch: []string{"terminals"},
	},
	{
		ID: "shell/timeout-background-cancel", About: "a command that went to the background is canceled: it runs on",
		Steps: []Step{
			Do(timed("r1", "sleep 3; echo late", 1000, agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_BACKGROUND)),
			Send(Cancel("r1")),
			Sleep(4 * time.Second),
			Do(Shell("r2", "echo ok")),
		},
		Watch: []string{"terminals"},
	},
	{
		ID: "shell/timeout-cancel", About: "a command that outlives its timeout is stopped",
		Steps: []Step{
			Do(timed("r1", "echo start; sleep 30; echo late", 1000, agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_CANCEL)),
			Do(Shell("r2", "pgrep -f 'sleep 30' >/dev/null; echo rc=$?")),
		},
	},
	{
		ID: "shell/timeout-default", About: "a timeout without a behavior",
		Steps: []Step{
			Do(timed("r1", "echo start; sleep 30; echo late", 1000, agentv1.TimeoutBehavior_TIMEOUT_BEHAVIOR_UNSPECIFIED)),
			Do(Shell("r2", "pgrep -f 'sleep 30' >/dev/null; echo rc=$?")),
		},
		Watch: []string{"terminals"},
	},
	{
		ID: "shell/hard-timeout", About: "a hard timeout ends the command",
		Steps: []Step{
			Do(ShellArgs("r1", &agentv1.ShellArgs{Command: "echo start; sleep 30; echo late", HardTimeout: proto.Int32(1000)})),
			Do(Shell("r2", "pgrep -f 'sleep 30' >/dev/null; echo rc=$?")),
		},
	},
	{
		ID: "shell/cancel", Ref: "C-4", About: "a running command is canceled",
		Steps: []Step{
			Send(Shell("r1", "echo start; sleep 60; echo done")),
			Sleep(time.Second),
			Send(Cancel("r1")),
			Sleep(time.Second),
			Do(Shell("r2", "pgrep -f 'sleep 60' >/dev/null; echo rc=$?")),
		},
	},
	{
		ID: "shell/parallel", Rules: anyExecID, Ref: "C-5", About: "four commands at once",
		Steps: []Step{Do(
			Shell("r1", "sleep 1; echo 1"), Shell("r2", "sleep 1; echo 2"), Shell("r3", "sleep 1; echo 3"), Shell("r4", "sleep 1; echo 4"),
		)},
	},
	{
		ID: "shell/background-job", Ref: "C-6", About: "a job the command leaves behind keeps running",
		Steps: []Step{
			Do(Shell("r1", "(sleep 1; echo alive > marker) & echo started")),
			Sleep(2 * time.Second),
			Do(Shell("r2", "cat marker")),
		},
	},
	{
		ID: "shell/is-background", About: "a command asked to run in the background",
		Steps: []Step{
			Do(ShellArgs("r1", &agentv1.ShellArgs{Command: "echo bg; sleep 1; echo end", IsBackground: true})),
			Sleep(2 * time.Second),
		},
		Watch: []string{"terminals"},
	},

	// zsh as the user's shell.
	{ID: "shell/zsh-echo", About: "zsh: stdout and exit code", Shell: zsh, Steps: []Step{Do(Shell("r1", "echo hello; echo $ZSH_NAME"))}},
	{
		ID: "shell/zsh-session", About: "zsh: directory, variables and functions stay", Shell: zsh,
		Steps: []Step{
			Do(Shell("r1", `mkdir -p sub && cd sub; export GROK_T1=value1; LOCAL_T=local; gt() { echo "fn:$1"; }`)),
			Do(Shell("r2", `pwd; echo "[$GROK_T1] [$LOCAL_T]"; gt ok`)),
		},
	},
	{
		ID: "shell/zsh-options", Ref: "B-5", About: "zsh: options", Shell: zsh,
		Steps: []Step{Do(Shell("r1", "setopt extendedglob")), Do(Shell("r2", "setopt | grep -c extendedglob"))},
	},
	{
		ID: "shell/zsh-zdotdir", Ref: "B-15", About: "zsh: ZDOTDIR is kept and its files are not sourced", Shell: zsh,
		Files: []File{Text("zd/.zshenv", "echo ZSHENV\n")},
		Steps: []Step{Do(Shell("r1", "export ZDOTDIR=$PWD/zd")), Do(Shell("r2", "echo cmd")), Do(Shell("r3", `echo "$ZDOTDIR"`))},
	},
	{
		ID: "shell/zsh-not-found", About: "zsh: a command that does not exist", Shell: zsh,
		Steps: []Step{Do(Shell("r1", "command_that_does_not_exist_xyz"))},
	},
}
