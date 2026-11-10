//go:build compare

package read_test

import (
	"encoding/json"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
)

// lines are what the random texts are made of: lines of one and of several
// UTF-16 units a character, both line ends, a byte order mark and no end.
var lines = []string{"a\n", "bc\n", "é\n", "🙂🙂\n", "long line of text\n", "\n", "x\r\n", "\r\n", "tail", "\ufeff", "日本\n", "\r"}

func randomText(r *rand.Rand) string {
	var b strings.Builder
	for range r.IntN(14) {
		b.WriteString(lines[r.IntN(len(lines))])
	}
	return b.String()
}

// randomArgs are read arguments around the size of the random texts.
func randomArgs(r *rand.Rand) map[string]any {
	args := map[string]any{}
	if r.IntN(2) == 0 {
		args["offset"] = r.IntN(24) - 8
	}
	if r.IntN(2) == 0 {
		args["limit"] = r.IntN(8)
	}
	if r.IntN(2) == 0 {
		args["maxChars"] = r.IntN(30)
	}
	return args
}

// The read of a text file gives what the desktop's reader gives, on random
// texts, ranges and max_chars.
func TestOracleRead(t *testing.T) {
	oracle := compattest.NewOracle(t)
	rd, root := newReader(t)
	r := rand.New(rand.NewPCG(7, 8))
	for range *compattest.OracleRuns {
		text, args := randomText(r), randomArgs(r)
		var want struct {
			Content      string
			TotalLines   int32
			Truncated    bool
			RangeApplied bool
		}
		oracle.Call(&want, "stream", text, args)
		raw, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		got := readText(t, rd, root, text, raw)
		if got == nil {
			t.Errorf("read %q %s: no success, the desktop says %+v", text, raw, want)
			continue
		}
		if got.GetContent() != want.Content || got.GetTotalLines() != want.TotalLines || got.GetTruncated() != want.Truncated ||
			got.GetRangeApplied() != want.RangeApplied {
			t.Errorf("read %q %s:\n got %q lines=%d truncated=%v range=%v\nwant %q lines=%d truncated=%v range=%v", text, raw,
				got.GetContent(), got.GetTotalLines(), got.GetTruncated(), got.GetRangeApplied(),
				want.Content, want.TotalLines, want.Truncated, want.RangeApplied)
		}
	}
}
