package read

import (
	"math"
	"slices"
	"strings"
	"unicode/utf8"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/pkg/textcodec"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/execstream"
)

const (
	// envelopeReserve leaves room in execstream.MaxJSON for the rest of a
	// result message.
	envelopeReserve = 4096
	maxTextRead     = execstream.MaxJSON - envelopeReserve
	// Binary data is base64-encoded in JSON.
	maxBinaryRead = maxTextRead * 3 / 4
	// mostlyCRLF is the share of CRLF line endings, in percent, above which
	// they are normalized to LF.
	mostlyCRLF = 5
	// readChunk is the size of the chunks the desktop reads text files in;
	// content beyond max_chars may run up to one chunk further.
	readChunk = 64 << 10
)

// fileText is a decoded text file.
type fileText struct {
	text string
	// base is the file offset from which text holds the file's bytes as they
	// are, or -1 if it was converted.
	base int
}

// decodeFile decodes text. Terminal files are read like any other, as on the
// desktop's local-exec.
func decodeFile(b []byte, hint string) (fileText, error) {
	text, base, err := textcodec.Decode(b, hint)
	return fileText{text: text, base: base}, err
}

// setTextRange stores the requested lines of text in r.
func setTextRange(r *agentv1.ReadSuccess, f fileText, x *agentv1.ReadArgs) {
	text := f.text
	normalize := strings.Count(text, "\r\n")*100 >= max(1, strings.Count(text, "\n"))*mostlyCRLF
	lines := splitLines(text)
	start, end := lineRange(len(lines), x.Offset, x.Limit)
	ranged := x.Offset != nil || x.Limit != nil
	r.TotalLines = int32(min(len(lines), math.MaxInt32)) //nolint:gosec // clamped
	r.RangeApplied = ranged && text != "" && (x.Offset == nil || int(x.GetOffset()) <= len(lines))
	selected := lines[start:end]
	units, byLines := maxCharsLimit(x, r.RangeApplied)
	if byLines {
		selected, r.Truncated = limitChars(lines, start, end, int(x.GetMaxChars()), f.base)
	}
	if normalize {
		selected = stripCR(lines, start, selected)
	}
	content := strings.Join(selected, "\n")
	if units >= 0 && textcodec.UTF16Len(content) > units {
		content, r.Truncated = textcodec.TruncateUTF16(content, units), true
	}
	if len(content) > maxTextRead {
		content = textcodec.TruncateUTF8(content, maxTextRead)
		r.Truncated = true
	}
	r.Output = &agentv1.ReadSuccess_Content{Content: content}
}

// maxCharsLimit returns how many UTF-16 code units of content max_chars
// leaves, or -1, and whether the lines are limited first as the desktop's
// streaming read does. Like the desktop, max_chars does not limit reads from
// the end, nor an offset past the end, which returns the whole text.
func maxCharsLimit(x *agentv1.ReadArgs, rangeApplied bool) (int, bool) {
	ranged := x.Offset != nil || x.Limit != nil
	switch {
	case x.MaxChars == nil:
		return -1, false
	case x.GetOffset() < 0 || ranged && !rangeApplied:
		return -1, false
	}
	return int(x.GetMaxChars()) + readChunk, true
}

// stripCR removes the CR of the CRLF endings of selected, the lines of
// lines from first on. A line that max_chars cut short has no ending.
func stripCR(lines []string, first int, selected []string) []string {
	out := slices.Clone(selected)
	for i, line := range out {
		if n := first + i; n < len(lines)-1 && len(line) == len(lines[n]) {
			out[i] = strings.TrimSuffix(line, "\r")
		}
	}
	return out
}

// limitChars returns lines[first:last] up to the line that takes their
// UTF-16 length past maxChars, as the desktop's streaming read does; the
// newlines between lines count, CRs of line endings do not. It reports
// whether more of the range was left out. When the lines hold the file's
// bytes from offset base on, the desktop's read chunks are followed: a line
// that went past maxChars by a chunk boundary inside it ends there.
func limitChars(lines []string, first, last, maxChars, base int) ([]string, bool) {
	at := base
	if base >= 0 {
		for _, line := range lines[:first] {
			at += len(line) + 1
		}
	}
	count := 0
	for i := first; i < last; i++ {
		line := lines[i]
		sep := min(i-first, 1)
		for b := (at/readChunk + 1) * readChunk; base >= 0 && b < at+len(line); b += readChunk {
			n := b - at
			for n > 0 && !utf8.RuneStart(line[n]) {
				n-- // a character the chunk cuts belongs to the next one
			}
			if n > 0 && count+sep+textcodec.UTF16Len(line[:n])-1 > maxChars {
				return append(lines[first:i:i], line[:n]), true
			}
		}
		count += sep + textcodec.UTF16Len(line)
		if i < len(lines)-1 && strings.HasSuffix(line, "\r") {
			count--
		}
		if count > maxChars {
			return lines[first : i+1], i+1 < last
		}
		at += len(line) + 1
	}
	return lines[first:last], false
}

// splitLines splits like the desktop: a trailing newline yields an empty
// last line.
func splitLines(s string) []string { return strings.Split(s, "\n") }

// lineRange converts a 1-based offset (negative counts from the end) and a
// line limit into slice bounds. Like the desktop, an offset past the end
// returns the whole text.
func lineRange(total int, offset *int32, limit *uint32) (start, end int) {
	if offset != nil {
		if o := int(*offset); o > 0 {
			start = o - 1
		} else if o < 0 {
			start = max(0, total+o)
		}
	}
	if start >= total {
		return 0, total
	}
	end = total
	if limit != nil {
		end = min(total, start+int(*limit))
	}
	return start, end
}
