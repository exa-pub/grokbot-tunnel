//go:build compare

package textcodec_test

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/exa-pub/grokbot-tunnel/internal/pkg/compattest"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
)

// pieces are what the random starts of files are made of: text of several
// scripts, bytes that end text, byte order marks and image signatures.
var pieces = [][]byte{
	[]byte("hello world\n"), []byte("line\r\n"), []byte("привет "), []byte("日本語"), []byte("🙂"), []byte("caf\xe9 "), {0}, {0, 0},
	{0xff}, {0xfe}, {0xff, 0xfe}, {0xfe, 0xff}, {0xef, 0xbb, 0xbf}, {0x1b, '[', '0', 'm'}, {0x01}, {0x7f}, {0xd0}, {0xe2, 0x82},
	[]byte("\x89PNG\r\n\x1a\n"), {0xff, 0xd8, 0xff}, []byte("GIF89a"), []byte("RIFF\x24\x00\x00\x00WEBPVP8 "), []byte("a"), []byte("\t"),
}

func utf16Bytes(s string, bigEndian bool) []byte {
	var out []byte
	for _, u := range utf16.Encode([]rune(s)) {
		if bigEndian {
			out = append(out, byte(u>>8), byte(u))
		} else {
			out = append(out, byte(u), byte(u>>8))
		}
	}
	return out
}

// randomStart makes the start of a file: pieces in a row, sometimes as
// UTF-16, sometimes as long as the chunk the desktop looks at.
func randomStart(r *rand.Rand) []byte {
	var b []byte
	for range r.IntN(12) {
		b = append(b, pieces[r.IntN(len(pieces))]...)
	}
	switch r.IntN(8) {
	case 0:
		b = utf16Bytes(strings.ToValidUTF8(string(b), "?"), r.IntN(2) == 0)
	case 1:
		// Around the 8192 bytes of the chunk, where a character may be cut.
		for len(b) < 8185+r.IntN(16) {
			b = append(b, pieces[r.IntN(5)]...)
		}
	}
	return b
}

// The start of a file is classified as the desktop classifies it, on random
// inputs. Where the desktop would ask jschardet the oracle has no answer.
func TestOracleFormat(t *testing.T) {
	oracle := compattest.NewOracle(t)
	r := rand.New(rand.NewPCG(1, 2))
	compared := 0
	for range *compattest.OracleRuns {
		raw := randomStart(r)
		var want *struct {
			Encoding                  string
			IsBinaryFile, IsImageFile bool
		}
		oracle.Call(&want, "format", compattest.Bytes(raw))
		if want == nil {
			continue
		}
		compared++
		encoding := want.Encoding
		if strings.EqualFold(encoding, "utf8") {
			encoding = textcodec.EncodingUTF8
		}
		got := textcodec.Detect(raw)
		if got != (textcodec.Format{Encoding: encoding, Binary: want.IsBinaryFile, Image: want.IsImageFile}) {
			t.Errorf("Detect(%x) = %+v, the desktop says %+v", raw, got, *want)
		}
	}
	if compared < *compattest.OracleRuns/2 {
		t.Errorf("only %d of %d inputs had an answer", compared, *compattest.OracleRuns)
	}
}

// Whether invalid UTF-8 counts as mostly UTF-8, on random inputs.
func TestOracleMostlyUTF8(t *testing.T) {
	oracle := compattest.NewOracle(t)
	r := rand.New(rand.NewPCG(3, 4))
	for range *compattest.OracleRuns {
		lossy := textcodec.LossyUTF8(randomStart(r))
		var want bool
		oracle.Call(&want, "mostlyUtf8", lossy)
		if got := textcodec.MostlyUTF8(lossy); got != want {
			t.Errorf("MostlyUTF8(%q) = %v, the desktop says %v", lossy, got, want)
		}
	}
}
