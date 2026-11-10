package scenarios

import (
	"fmt"

	"google.golang.org/protobuf/proto"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	. "github.com/exa-pub/grokbot-tunnel/tests/compare/scenario" //nolint:revive,staticcheck // scenarios read as a list of steps
	"github.com/exa-pub/grokbot-tunnel/tests/compare/transcript"
)

// ripgrep lets the desktop's listing run: as released it fails before it
// starts (ls/as-released).
var ripgrep = []string{"ripgrep"}

// lists is a scenario that lists the fixtures in several ways.
func lists(id, ref, about string, files []File, args ...*agentv1.LsArgs) Scenario {
	s := Scenario{ID: id, Ref: ref, About: about, Files: files, Patches: ripgrep}
	for i, a := range args {
		s.Steps = append(s.Steps, Do(LsArgs("r"+string(rune('1'+i)), a)))
	}
	return s
}

func dir(p string) *agentv1.LsArgs { return &agentv1.LsArgs{Path: p} }

func many(n int) []File {
	files := make([]File, n)
	for i := range files {
		files[i] = Text(fmt.Sprintf("big/f%05d.txt", i), "")
	}
	return files
}

var tree = []File{
	Text("tree/a/b/f1.txt", ""), Text("tree/a/f2.md", ""), Text("tree/c/f3.go", ""), Text("tree/.hidden", ""),
	Text("tree/.gitignore", "ignored/\n"), Text("tree/ignored/x", ""), Dir("tree/.git"), Text("tree/.git/HEAD", "ref: refs/heads/main\n"),
}

var ls = []Scenario{
	{
		ID: "ls/sandbox", About: "a listing with a sandbox policy, which is refused",
		Files: []File{Text("dir/a.txt", "a")}, Patches: ripgrep,
		Steps: []Step{
			Do(ExecJSON("r1", `{"id":7,"lsArgs":{"path":"$ROOT/dir","toolCallId":"tool-1",`+
				`"sandboxPolicy":{"type":"TYPE_WORKSPACE_READONLY"}}}`)),
			Do(ExecJSON("r2", `{"id":7,"readArgs":{"path":"$ROOT/dir/a.txt","toolCallId":"tool-2"}}`)),
		},
	},
	{
		ID: "ls/as-released", About: "the released desktop cannot list: its ripgrep path is never set",
		Files: []File{Text("a.md", "a")},
		Steps: []Step{Do(Ls("r1", "."))},
	},
	lists("ls/tree", "", "the shape and order of a small tree",
		[]File{
			Text("b.txt", "b"), Text("a.md", "a"), Text("src/main.go", "package main\n"), Text("src/util/x.go", "package util\n"),
			Dir("empty"), Text("Zeta.TXT", "z"), Text("_under", ""), Text("10.txt", ""), Text("9.txt", ""), Text("ä.txt", ""),
		}, dir(".")),
	lists("ls/gitignore", "H-1", "hidden files and .gitignore", tree, dir("tree")),
	lists("ls/paths", "H-2", "the ways to name a directory", tree,
		dir("tree/a"), dir("$ROOT/tree/a"), dir("tree/a/"), dir("./tree/./a"), dir("")),
	lists("ls/missing", "H-3", "a directory that does not exist", tree, dir("tree/zzz")),
	lists("ls/file", "H-4", "a file instead of a directory", tree, dir("tree/c/f3.go")),
	func() Scenario {
		s := lists("ls/big", "H-5", "a directory of 3000 files is listed in part", many(3000), dir("big"), dir("."))
		// Which of the files are listed depends on the order ripgrep finds
		// them in; how many is the behavior.
		s.Rules = []transcript.Rule{{Path: "requests/**/childrenFiles", Func: func(v any) any {
			files, ok := v.([]any)
			if !ok {
				return v
			}
			return fmt.Sprintf("<%d files>", len(files))
		}}}
		return s
	}(),
	lists("ls/ignore", "", "the ignore patterns of the request", tree,
		&agentv1.LsArgs{Path: "tree", Ignore: []string{"*.md"}},
		&agentv1.LsArgs{Path: "tree", Ignore: []string{"a"}},
		&agentv1.LsArgs{Path: "tree", Ignore: []string{"a/b", "**/*.go"}},
		&agentv1.LsArgs{Path: "tree", Ignore: []string{"!ignored"}}),
	lists("ls/empty", "", "empty directories", []File{Dir("empty"), Dir("outer/inner")}, dir("empty"), dir("outer")),
	lists("ls/deep", "", "a deep tree",
		[]File{Text("d1/d2/d3/d4/d5/d6/d7/d8/leaf.txt", ""), Text("d1/top.txt", "")}, dir("."), dir("d1/d2/d3/d4")),
	lists("ls/links", "J-8", "symbolic links to a directory and to a file",
		[]File{Text("inner/i.txt", "in"), Link("innerlink", "inner"), Link("filelink", "inner/i.txt"), Link("dangling", "nowhere")},
		dir("."), dir("innerlink")),
	lists("ls/unreadable", "", "a directory the user may not read",
		[]File{Text("ok/a.txt", ""), Text("locked/b.txt", ""), {Path: "locked", Dir: true, Mode: 0o000}}, dir("."), dir("locked")),
	lists("ls/timeout", "", "a timeout of the request",
		tree, &agentv1.LsArgs{Path: "tree", TimeoutMs: proto.Uint32(0)}, &agentv1.LsArgs{Path: "tree", TimeoutMs: proto.Uint32(5000)}),
	lists("ls/extensions", "", "the extension counts of a tree",
		[]File{
			Text("x/a.go", ""), Text("x/b.go", ""), Text("x/c.md", ""), Text("x/Makefile", ""), Text("x/.env", ""),
			Text("x/y/d.tar.gz", ""),
		},
		dir("."), dir("x")),
	lists("ls/terminals", "", "terminal files carry what they ran",
		[]File{Text("terminals/5.txt", terminalFile), Text("terminals/notes.txt", "plain")}, dir("terminals"), dir(".")),
}
