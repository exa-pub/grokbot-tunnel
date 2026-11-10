package scenarios

import (
	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	. "github.com/exa-pub/grokbot-tunnel/tests/compare/scenario" //nolint:revive,staticcheck // scenarios read as a list of steps
)

// outside are files next to the root, in the home directory.
var outside = []File{Text("outside/s.txt", "secret\n"), Dir("outside/dir")}

var inside = []File{
	Text("inner/i.txt", "in\n"),
	Link("esc.txt", "$DIR/../home/outside/s.txt"), Link("escdir", "$DIR/../home/outside"),
	Link("abs.txt", "$DIR/inner/i.txt"), Link("rel.txt", "../root/inner/i.txt"), Link("innerlink", "inner"),
	Link("loop", "loop"),
}

var root = []Scenario{
	{
		ID: "root/read", Ref: "J-2", About: "reads inside and outside the root", Files: inside, Home: outside,
		Steps: []Step{
			Do(Read("r1", "inner/i.txt")), Do(Read("r2", "$HOME/outside/s.txt")), Do(Read("r3", "~/outside/s.txt")),
			Do(Read("r4", "inner/../../home/outside/s.txt")), Do(Read("r5", "../home/outside/s.txt")),
			Do(Read("r6", "/etc/hostname")), Do(Read("r7", "$ROOT/../root/inner/i.txt")), Do(Read("r8", "$ROOT")),
		},
	},
	{
		ID: "root/links", Ref: "J-6", About: "symbolic links that leave the root and that stay in it", Files: inside, Home: outside,
		Steps: []Step{
			Do(Read("r1", "esc.txt")), Do(Read("r2", "abs.txt")), Do(Read("r3", "rel.txt")), Do(Read("r4", "innerlink/i.txt")),
			Do(Read("r5", "escdir/s.txt")), Do(Read("r6", "loop")),
		},
	},
	{
		ID: "root/shell", Ref: "J-5", About: "the root does not limit commands", Files: inside, Home: outside,
		Steps: []Step{Do(Shell("r1", "pwd; cat ~/outside/s.txt")), Do(Shell("r2", "cd /tmp && pwd")), Do(Shell("r3", "pwd"))},
	},
	{
		ID: "root/transfer", Ref: "J-10", About: "uploads and downloads outside the root", Files: inside, Home: outside,
		Steps: []Step{
			Do(Upload("r1", "$HOME/outside/up.txt", []byte("x"))), Do(Upload("r2", "../home/outside/up2.txt", []byte("x"))),
			Do(Upload("r3", "escdir/up3.txt", []byte("x"))), Do(Upload("r4", "esc.txt", []byte("x"))),
			Do(Download("r5", "$HOME/outside/s.txt")), Do(Download("r6", "esc.txt")), Do(Download("r7", "escdir/s.txt")),
			Do(Upload("r8", "~/tilde.txt", []byte("x"))),
		},
		Watch: []string{"."},
	},
	{
		ID: "root/ls", About: "listings outside the root", Files: inside, Home: outside, Patches: ripgrep,
		Steps: []Step{Do(Ls("r1", "$HOME/outside")), Do(Ls("r2", "..")), Do(Ls("r3", "escdir")), Do(Ls("r4", "/"))},
	},
	{
		ID: "root/background", About: "a background command with a working directory outside the root", Home: outside,
		Steps: []Step{Do(Background("r1", &agentv1.BackgroundShellSpawnArgs{Command: "pwd", WorkingDirectory: "$HOME/outside"}))},
	},
	{
		ID: "root/linked", Ref: "J-11", About: "a root that is a symbolic link", RootLink: true,
		Files: []File{Text("VERSION", "v1\n")},
		Steps: []Step{
			Do(Shell("r1", "pwd; pwd -P")), Do(Read("r2", "VERSION")), Do(Read("r3", "$ROOT/VERSION")), Do(Read("r4", "$REAL/VERSION")),
			Do(Upload("r5", "new.txt", []byte("x"))), Do(Download("r6", "$REAL/VERSION")),
		},
	},
	{
		ID: "root/linked-ls", About: "a listing of a root that is a symbolic link", RootLink: true, Patches: ripgrep,
		Files: []File{Text("VERSION", "v1\n"), Text("sub/a.txt", "")},
		Steps: []Step{Do(Ls("r1", ".")), Do(Ls("r2", "$ROOT/sub")), Do(Ls("r3", "$REAL"))},
	},
}
