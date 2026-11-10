package scenarios

import (
	. "github.com/exa-pub/grokbot-tunnel/tests/compare/scenario" //nolint:revive,staticcheck // scenarios read as a list of steps
)

var transfer = []Scenario{
	{
		ID: "transfer/upload-download", About: "a file written by upload and read back by download",
		Steps: []Step{
			Do(Upload("r1", "in/data.bin", []byte("upload-ok\n"))),
			Do(Download("r2", "in/data.bin")),
		},
		Watch: []string{"in"},
	},
	{
		ID: "transfer/upload-paths", Ref: "I-1", About: "upload makes the directories and takes absolute paths",
		Steps: []Step{
			Do(Upload("r1", "up/new/dir/hello.txt", []byte("uploaded"))),
			Do(Upload("r2", "$ROOT/up/abs.txt", []byte("uploaded"))),
			Do(Upload("r3", "./up/./dot.txt", []byte("uploaded"))),
			Do(Upload("r4", "", []byte("uploaded"))),
			Do(Upload("r5", "up/empty.txt", nil)),
		},
		Watch: []string{"up"},
	},
	{
		ID: "transfer/upload-overwrite", Ref: "I-2", About: "an overwritten file keeps its mode",
		Files: []File{Text("up/hello.txt", "old").WithMode(0o640), Text("up/ro.txt", "old").WithMode(0o444)},
		Steps: []Step{Do(Upload("r1", "up/hello.txt", []byte("new"))), Do(Upload("r2", "up/ro.txt", []byte("new")))},
		Watch: []string{"up"},
	},
	{
		ID: "transfer/upload-link", Ref: "I-7", About: "an upload through a symbolic link writes its target",
		Files: []File{Text("up/target.txt", "old"), Link("up/link.txt", "target.txt"), Link("up/dangling.txt", "missing.txt")},
		Steps: []Step{Do(Upload("r1", "up/link.txt", []byte("new"))), Do(Upload("r2", "up/dangling.txt", []byte("new")))},
		Watch: []string{"up"},
	},
	{
		ID: "transfer/upload-errors", About: "uploads that cannot be written",
		Files: []File{Dir("up/dir"), Text("up/file", "x"), {Path: "up/locked", Dir: true, Mode: 0o555}},
		Steps: []Step{
			Do(Upload("r1", "up/dir", []byte("x"))),
			Do(Upload("r2", "up/file/below", []byte("x"))),
			Do(Upload("r3", "up/locked/new.txt", []byte("x"))),
		},
		Watch: []string{"up"},
	},
	{
		ID: "transfer/upload-large", About: "an upload of 5 MB",
		Steps: []Step{Do(Upload("r1", "up/5m.bin", noise(5<<20)))},
		Watch: []string{"up"},
	},
	{
		ID: "transfer/download", Ref: "I-3", About: "downloads of text, bytes and nothing",
		Files: []File{Text("r/lines.txt", fiveLines), Bytes("r/bin.dat", noise(4096)), Text("r/empty", "")},
		Steps: []Step{
			Do(Download("r1", "r/lines.txt")), Do(Download("r2", "r/bin.dat")), Do(Download("r3", "r/empty")),
			Do(Download("r4", "$ROOT/r/lines.txt")),
		},
	},
	{
		ID: "transfer/download-large", Ref: "I-5", About: "a download of 5 MB comes in chunks",
		Files: []File{Bytes("r/5m.bin", noise(5<<20)), Bytes("r/1m.bin", noise(1<<20)), Bytes("r/1m1.bin", noise(1<<20+1))},
		Steps: []Step{Do(Download("r1", "r/5m.bin")), Do(Download("r2", "r/1m.bin")), Do(Download("r3", "r/1m1.bin"))},
	},
	{
		ID: "transfer/download-errors", Ref: "I-6", About: "downloads that cannot be read",
		Files: []File{Dir("r/dir"), Text("r/secret", "x").WithMode(0), Sparse("r/huge.bin", 101<<20), Link("r/dangling", "missing")},
		Steps: []Step{
			Do(Download("r1", "r/missing.txt")), Do(Download("r2", "r/dir")), Do(Download("r3", "r/secret")),
			Do(Download("r4", "r/huge.bin")), Do(Download("r5", "r/dangling")), Do(Download("r6", "")),
		},
	},
}
