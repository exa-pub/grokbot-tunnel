package scenarios

import (
	"time"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	. "github.com/exa-pub/grokbot-tunnel/tests/compare/scenario" //nolint:revive,staticcheck // scenarios read as a list of steps
)

func spawn(id, command string) Frame {
	return Background(id, &agentv1.BackgroundShellSpawnArgs{Command: command})
}

var terminals = []string{"terminals"}

var background = []Scenario{
	{
		ID: "background/ticks", Ref: "E-1", About: "a background command writes its terminal file and ends",
		Steps: []Step{Do(spawn("r1", "for i in 1 2 3; do echo tick $i; sleep 0.3; done")), Sleep(3 * time.Second)},
		Watch: terminals,
	},
	{
		ID: "background/exit-code", Ref: "E-5", About: "the exit code goes to the footer",
		Steps: []Step{Do(spawn("r1", "echo out; sleep 0.3; echo err >&2; exit 7")), Sleep(2 * time.Second)},
		Watch: terminals,
	},
	{
		ID: "background/running", Ref: "E-3", About: "a command that is still running",
		Steps: []Step{
			Do(spawn("r1", "echo up; sleep 60")), Sleep(2 * time.Second),
			Do(Shell("r2", "pgrep -f 'sleep 60' >/dev/null; echo rc=$?")),
		},
		Watch: terminals,
	},
	{
		ID: "background/killed", Ref: "E-4", About: "a command that is killed",
		Steps: []Step{
			Do(spawn("r1", "echo up; sleep 61")), Sleep(time.Second),
			Do(Shell("r2", "pkill -f 'sleep 61'; echo rc=$?")), Sleep(2 * time.Second),
		},
		Watch: terminals,
	},
	{
		ID: "background/cwd-missing", Ref: "E-6", About: "a working directory that does not exist",
		Steps: []Step{
			Do(Background("r1", &agentv1.BackgroundShellSpawnArgs{Command: "pwd", WorkingDirectory: "/nonexistent/x"})),
			Sleep(2 * time.Second),
		},
		Watch: terminals,
	},
	{
		ID: "background/cwd", About: "a working directory inside the root",
		Files: []File{Dir("work")},
		Steps: []Step{
			Do(Background("r1", &agentv1.BackgroundShellSpawnArgs{Command: "pwd", WorkingDirectory: "work"})),
			Sleep(2 * time.Second),
		},
		Watch: terminals,
	},
	{
		ID: "background/session", Ref: "E-7", About: "what a background command changes does not reach the session",
		Steps: []Step{
			Do(Shell("r1", "export BEFORE=1; mkdir -p sub; cd sub")),
			Do(spawn("r2", `echo "[$BEFORE]"; pwd; export BG_T=1; cd /tmp`)), Sleep(2 * time.Second),
			Do(Shell("r3", `echo "[$BG_T]"; pwd`)),
		},
		Watch: terminals,
	},
	{
		ID: "background/two", About: "two background commands get a terminal file each",
		Steps: []Step{Do(spawn("r1", "echo first")), Do(spawn("r2", "echo second")), Sleep(2 * time.Second)},
		Watch: terminals,
	},
	{
		ID: "background/output", About: "UTF-8 output and a long line",
		Steps: []Step{
			Do(spawn("r1", `printf 'Привет 🙂\n'; head -c 100000 /dev/zero | tr '\0' 'x'; echo`)),
			Sleep(2 * time.Second),
		},
		Watch: terminals,
	},
	{
		ID: "background/output-bytes", About: "output that is not UTF-8",
		Steps: []Step{Do(spawn("r1", `printf 'caf\xe9 \x80 raw\n'; echo next`)), Sleep(2 * time.Second)},
		Watch: terminals,
	},
	{
		ID: "background/output-bom", About: "output that starts with a UTF-16 byte order mark",
		Steps: []Step{Do(spawn("r1", `printf '\xff\xfe raw\n'; echo next`)), Sleep(2 * time.Second)},
		Watch: terminals,
	},
	{
		ID: "background/terminals-exist", About: "terminal files that are there already",
		Files: []File{Text("terminals/1.txt", "old\n"), Text("terminals/notes.md", "keep\n")},
		Steps: []Step{Do(spawn("r1", "echo new")), Sleep(2 * time.Second)},
		Watch: terminals,
	},
	{
		ID: "background/empty-command", About: "an empty command",
		Steps: []Step{Do(spawn("r1", "")), Sleep(time.Second)},
		Watch: terminals,
	},
}
