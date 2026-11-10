package scenarios

import (
	"google.golang.org/protobuf/proto"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	. "github.com/exa-pub/grokbot-tunnel/tests/compare/scenario" //nolint:revive,staticcheck // scenarios read as a list of steps
)

const fiveLines = "line1\nline2\nline3\nline4\nline5\n"

// reads is a scenario that reads one fixture in several ways, each a request
// of its own.
func reads(id, ref, about string, files []File, args ...*agentv1.ReadArgs) Scenario {
	s := Scenario{ID: id, Ref: ref, About: about, Files: files}
	for i, a := range args {
		s.Steps = append(s.Steps, Do(ReadArgs("r"+string(rune('1'+i)), a)))
	}
	return s
}

func path(p string) *agentv1.ReadArgs { return &agentv1.ReadArgs{Path: p} }

func ranged(p string, offset int32, limit uint32) *agentv1.ReadArgs {
	a := &agentv1.ReadArgs{Path: p, Offset: proto.Int32(offset)}
	if limit > 0 {
		a.Limit = proto.Uint32(limit)
	}
	return a
}

// terminalFile is what a finished background command leaves, written by
// hand so that the read of it is the same on every run.
const terminalFile = "---\npid: 4242\ncwd: \"/work\"\ncommand: \"echo tick\"\nstatus: succeeded\n" +
	"started_at: 2026-01-01T00:00:00.000Z\nrunning_for_ms: 1000     \n---\ntick\n\n---\nexit_code: 0\nelapsed_ms: 1000\n" +
	"ended_at: 2026-01-01T00:00:01.000Z\n---\n"

var read = []Scenario{
	reads("read/text", "F-1", "a short text file, whole", []File{Text("lines.txt", fiveLines)}, path("lines.txt")),
	reads("read/range", "F-2", "offset and limit", []File{Text("lines.txt", fiveLines)},
		ranged("lines.txt", 2, 2), ranged("lines.txt", 1, 1), ranged("lines.txt", 5, 10), ranged("lines.txt", 0, 2),
		&agentv1.ReadArgs{Path: "lines.txt", Limit: proto.Uint32(2)}, ranged("lines.txt", 6, 0), ranged("lines.txt", 7, 0),
		ranged("lines.txt", 100, 0)),
	reads("read/negative-offset", "F-3", "an offset counted from the end", []File{Text("lines.txt", fiveLines)},
		ranged("lines.txt", -2, 0), ranged("lines.txt", -1, 0), ranged("lines.txt", -3, 1), ranged("lines.txt", -100, 0)),
	reads("read/max-chars", "", "the answer is cut at max_chars", []File{Text("lines.txt", fiveLines)},
		&agentv1.ReadArgs{Path: "lines.txt", MaxChars: proto.Uint32(8)},
		&agentv1.ReadArgs{Path: "lines.txt", MaxChars: proto.Uint32(6)},
		&agentv1.ReadArgs{Path: "lines.txt", MaxChars: proto.Uint32(1000)},
		&agentv1.ReadArgs{Path: "lines.txt", MaxChars: proto.Uint32(0)},
		&agentv1.ReadArgs{Path: "lines.txt", Offset: proto.Int32(2), Limit: proto.Uint32(3), MaxChars: proto.Uint32(8)}),
	reads("read/crlf", "F-4", "CRLF line ends", []File{Text("crlf.txt", "a\r\nb\r\nc\r\n")}, path("crlf.txt"), ranged("crlf.txt", 2, 1)),
	reads("read/bom", "F-5", "a UTF-8 byte order mark", []File{Bytes("bom8.txt", []byte("\xef\xbb\xbfbom-utf8\n"))}, path("bom8.txt")),
	reads("read/utf16", "F-6", "UTF-16 with and without a byte order mark",
		[]File{
			Bytes("le-bom.txt", []byte("\xff\xfeh\x00i\x00\n\x00")), Bytes("be-bom.txt", []byte("\xfe\xff\x00h\x00i\x00\n")),
			Bytes("le.txt", []byte("h\x00e\x00l\x00l\x00o\x00 \x00w\x00o\x00r\x00l\x00d\x00\n\x00")),
		},
		path("le-bom.txt"), path("be-bom.txt"), path("le.txt")),
	reads("read/latin1", "F-7", "bytes that are not UTF-8",
		[]File{Bytes("latin1.txt", []byte("caf\xe9\n")), Bytes("mixed.txt", []byte("привет \xff мир\n"))},
		path("latin1.txt"), path("mixed.txt")),
	reads("read/encoding-hint", "", "a hint of the encoding",
		[]File{Bytes("latin1.txt", []byte("caf\xe9\n"))},
		&agentv1.ReadArgs{Path: "latin1.txt", EncodingHint: proto.String("latin1")},
		&agentv1.ReadArgs{Path: "latin1.txt", EncodingHint: proto.String("utf8")}),
	reads("read/empty", "F-8", "an empty file", []File{Text("empty.txt", "")}, path("empty.txt"), ranged("empty.txt", 1, 1)),
	reads("read/binary", "F-9", "a file that is not text", []File{Bytes("bin.dat", noise(4096)), Bytes("nul.txt", []byte("ab\x00cd\n"))},
		path("bin.dat"), path("nul.txt")),
	reads("read/big", "F-10", "a file of 14 MB", []File{Seq("big.txt", 2000000)},
		path("big.txt"), ranged("big.txt", 1999999, 0), ranged("big.txt", 1000000, 3)),
	reads("read/long-lines", "", "lines longer than a read chunk",
		[]File{{Path: "long.txt", Data: []byte("x"), Repeat: 300000}, Text("two.txt", "")},
		path("long.txt"), &agentv1.ReadArgs{Path: "long.txt", MaxChars: proto.Uint32(100)}),
	reads("read/no-final-newline", "F-11", "no newline at the end", []File{Text("nonl.txt", "no newline at end")}, path("nonl.txt")),
	reads("read/missing", "F-12", "a file that does not exist", nil, path("does-not-exist.txt"), path("no/such/dir/file.txt")),
	reads("read/directory", "F-13", "a directory", []File{Dir("r")}, path("r"), path(".")),
	reads("read/paths", "F-14", "the ways to name a file",
		[]File{Text("r/lines.txt", fiveLines)},
		path("r/lines.txt"), path("$ROOT/r/lines.txt"), path("file://$ROOT/r/lines.txt"), path("./r/../r/lines.txt"),
		path("r//lines.txt"), path("r/lines.txt/"), path(""), path("~/lines.txt")),
	reads("read/unreadable", "", "a file the user may not read", []File{Text("secret.txt", "x").WithMode(0)}, path("secret.txt")),
	reads("read/digest", "", "only the digest of a file", []File{Text("lines.txt", fiveLines), Bytes("bin.dat", noise(4096))},
		&agentv1.ReadArgs{Path: "lines.txt", DigestOnly: proto.Bool(true)},
		&agentv1.ReadArgs{Path: "bin.dat", DigestOnly: proto.Bool(true)},
		&agentv1.ReadArgs{Path: "missing", DigestOnly: proto.Bool(true)}),
	reads("read/too-large", "", "a file over the size limit", []File{Sparse("huge.bin", 101<<20), Sparse("zeros.bin", 1<<20)},
		path("huge.bin"), path("zeros.bin")),
	reads("read/special", "", "a FIFO-like path and a device", nil, path("/dev/null"), path("/proc/self/status")),
	reads("read/terminal-file", "E-2", "a terminal file: header, output and footer",
		[]File{Text("terminals/5.txt", terminalFile), Text("terminals/notes.txt", terminalFile), Text("copy.txt", terminalFile)},
		path("terminals/5.txt"), ranged("terminals/5.txt", 2, 3), path("terminals/notes.txt"), path("copy.txt"),
		&agentv1.ReadArgs{Path: "terminals/5.txt", MaxChars: proto.Uint32(40)}),

	// Images.
	reads("read/png", "G-1", "a small PNG", []File{Bytes("small.png", image("small.png"))}, path("small.png")),
	reads("read/png-large", "G-2", "a PNG larger than the model takes",
		[]File{Bytes("large.png", image("large.png"))}, path("large.png")),
	reads("read/jpeg", "G-3", "JPEG files, small and large",
		[]File{Bytes("small.jpg", image("small.jpg")), Bytes("big.jpg", image("big.jpg"))}, path("small.jpg"), path("big.jpg")),
	reads("read/gif-webp", "", "GIF and WebP",
		[]File{Bytes("a.gif", image("small.gif")), Bytes("a.webp", image("pixel.webp"))}, path("a.gif"), path("a.webp")),
	reads("read/fake-image", "G-5", "an image is told by its content, not its name",
		[]File{Text("fake.png", "not a png"), Bytes("image.txt", image("tiny.png")), Bytes("cut.png", image("small.png")[:40])},
		path("fake.png"), path("image.txt"), path("cut.png")),
	reads("read/image-range", "", "a range of an image", []File{Bytes("small.png", image("small.png"))},
		ranged("small.png", 2, 2), &agentv1.ReadArgs{Path: "small.png", DigestOnly: proto.Bool(true)}),
}
