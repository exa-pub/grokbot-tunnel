package read_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/png"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentv1 "github.com/exa-pub/grokbot-tunnel/internal/gen/agent/v1"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/pathroot"
	"github.com/exa-pub/grokbot-tunnel/internal/service/localexec/tool/read"
)

func readFile(t testing.TB, rd *read.Executor, x *agentv1.ReadArgs) *agentv1.ReadResult {
	t.Helper()
	var got *agentv1.ExecClientMessage
	require.NoError(t, rd.Execute(context.Background(), x, func(m *agentv1.ExecClientMessage) error { got = m; return nil }))
	return got.GetReadResult()
}

func write(t testing.TB, root *pathroot.Root, name string, data []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root.Dir(), name), data, 0o600))
}

func ptr[T any](v T) *T { return &v }

// A read of a few lines does not hold the file in memory, whether the file
// is UTF-8 or read again as Latin-1 (bug F1: a 100 MiB file took up to
// 1 GiB of allocations).
func TestReadLimitDoesNotLoadFile(t *testing.T) {
	rd, root := newReader(t)
	for _, line := range []string{"abcdefghijklmnop\n", "abcdefghijklmn\xe9\n"} {
		data := bytes.Repeat([]byte(line), (100<<20)/len(line))
		write(t, root, "big", data)
		size := len(data)
		data = nil //nolint:ineffassign,wastedassign // let the GC take it before measuring
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got := readFile(t, rd, &agentv1.ReadArgs{Path: "big", Limit: ptr[uint32](10)}).GetSuccess()
		runtime.ReadMemStats(&after)
		require.NotNil(t, got)
		assert.Equal(t, int32(size/len(line)+1), got.TotalLines)
		assert.Equal(t, 10, strings.Count(got.GetContent(), "\n")+1)
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 8<<20 {
			t.Errorf("%q: %d MiB allocated for a %d MiB file", line, alloc>>20, size>>20)
		}
	}
}

func BenchmarkReadLimit(b *testing.B) {
	rd, root := newReader(b)
	write(b, root, "big", bytes.Repeat([]byte("abcdefghijklmnop\n"), (64<<20)/17))
	b.ReportAllocs()
	for b.Loop() {
		readFile(b, rd, &agentv1.ReadArgs{Path: "big", Limit: ptr[uint32](10)})
	}
}

// Binary data is recognized in the first 8 KiB before an encoding hint
// applies; UTF-16 and UTF-32 are recognized without a byte order mark; a
// UTF-8 BOM before invalid UTF-8 stays as Latin-1 text (F4, F10).
func TestReadDetectsEncoding(t *testing.T) {
	rd, root := newReader(t)
	for _, tc := range []struct {
		name string
		data string
		hint *string
		want string
	}{
		{"utf-16le", "h\x00e\x00l\x00l\x00o\x00", nil, "hello"},
		{"utf-32le bom", "\xff\xfe\x00\x00h\x00\x00\x00i\x00\x00\x00", nil, "hi"},
		{"nul past 8 KiB", strings.Repeat("a", 9000) + "\x00", nil, strings.Repeat("a", 9000) + "\x00"},
		{"latin1 keeps a utf-8 bom", "\xef\xbb\xbf\xff\na", nil, "ï»¿ÿ\na"},
		{"hint", "caf\xe9", ptr("latin1"), "café"},
	} {
		write(t, root, "f", []byte(tc.data))
		got := readFile(t, rd, &agentv1.ReadArgs{Path: "f", EncodingHint: tc.hint})
		assert.Equal(t, tc.want, got.GetSuccess().GetContent(), tc.name)
	}
	for _, tc := range []struct {
		name, data string
		hint       *string
	}{
		{"nul with a hint", "ab\x00cd", ptr("latin1")},
		{"colored log", strings.Repeat("\x1b[31mred\x1b[0m\n", 10), nil},
	} {
		write(t, root, "f", []byte(tc.data))
		assert.Equal(t, "File content is binary data, not text", readFile(t, rd, &agentv1.ReadArgs{Path: "f", EncodingHint: tc.hint}).GetInvalidFile().GetReason(), tc.name)
	}
}

func pngImage(t testing.TB, w, h int, noise bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // test pixels
	for y := range h {
		for x := range w {
			c := color.NRGBA{uint8(x), uint8(y), 128, 255} //nolint:gosec // wraps on purpose
			if noise {
				c = color.NRGBA{uint8(r.UintN(256)), uint8(r.UintN(256)), uint8(r.UintN(256)), 255} //nolint:gosec // < 256
			}
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, img))
	return b.Bytes()
}

// Images are found by their signature, scaled down to the desktop's limits
// and checked; a PDF goes by its extension, a video as binary data with its
// extension (F3).
func TestReadImages(t *testing.T) {
	rd, root := newReader(t)
	small := pngImage(t, 40, 30, false)
	write(t, root, "noext", small)
	got := readFile(t, rd, &agentv1.ReadArgs{Path: "noext"}).GetSuccess()
	require.NotNil(t, got)
	assert.Equal(t, small, got.GetData(), "a small image is passed on as it is")
	assert.Equal(t, int64(len(small)), got.FileSize)

	write(t, root, "big.png", pngImage(t, 1500, 1200, true))
	got = readFile(t, rd, &agentv1.ReadArgs{Path: "big.png"}).GetSuccess()
	require.NotNil(t, got)
	cfg, format, err := image.DecodeConfig(bytes.NewReader(got.GetData()))
	require.NoError(t, err)
	assert.Equal(t, "png", format)
	assert.LessOrEqual(t, max(cfg.Width, cfg.Height), 1024)
	assert.LessOrEqual(t, len(got.GetData()), 1<<20)
	assert.InDelta(t, 1.25, float64(cfg.Width)/float64(cfg.Height), 0.01)

	write(t, root, "cut.png", small[:len(small)-30])
	reason := readFile(t, rd, &agentv1.ReadArgs{Path: "cut.png"}).GetInvalidFile().GetReason()
	assert.Equal(t, "File is not a valid image: its data could not be decoded (unexpected end of file). "+
		"A truncated or hand-copied file does this; write it again from the original bytes with code.", reason)

	write(t, root, "fake.png", []byte("just text"))
	assert.Equal(t, "just text", readFile(t, rd, &agentv1.ReadArgs{Path: "fake.png"}).GetSuccess().GetContent())

	for _, name := range []string{"clip.mp4", "doc.pdf"} {
		write(t, root, name, []byte("\x00\x01binary"))
		assert.Equal(t, []byte("\x00\x01binary"), readFile(t, rd, &agentv1.ReadArgs{Path: name}).GetSuccess().GetData(), name)
	}
	write(t, root, "notes.mp4", []byte("text"))
	assert.Equal(t, "text", readFile(t, rd, &agentv1.ReadArgs{Path: "notes.mp4"}).GetSuccess().GetContent())
}

// Other failures read as Node's error texts, as the desktop passes them on
// (F13).
func TestReadNodeErrors(t *testing.T) {
	rd, root := newReader(t)
	write(t, root, "file", []byte("x"))
	got := readFile(t, rd, &agentv1.ReadArgs{Path: "file/sub"}).GetError()
	assert.Equal(t, "ENOTDIR: not a directory, stat '"+filepath.Join(root.Dir(), "file/sub")+"'", got.GetError())
	require.NoError(t, syscall.Mkfifo(filepath.Join(root.Dir(), "fifo"), 0o600))
	assert.Equal(t, "Path is neither a file nor a directory", readFile(t, rd, &agentv1.ReadArgs{Path: "fifo"}).GetInvalidFile().GetReason())
}

// WEBP samples: a lossless 1×1 and an animated 1×1 (Modernizr's).
const (
	webpLossless = "UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA=="
	webpAnimated = "UklGRlIAAABXRUJQVlA4WAoAAAASAAAAAAAAAAAAQU5JTQYAAAD/////AABBTk1GJgAAAAAAAAAAAAAAAAAAAGQAAABWUDhMDQAAAC8AAAAQBxAREYiI/gcA"
)

func webpSample(t *testing.T, sample string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(sample)
	require.NoError(t, err)
	return b
}

// A WEBP that fits 1280 pixels and 1 MiB by its header is passed on as it
// is without decoding, as on the desktop, where no WEBP codec is
// registered: an animated or cut one too (F2-2).
func TestReadSmallWebPAsIs(t *testing.T) {
	rd, root := newReader(t)
	lossless := webpSample(t, webpLossless)
	for name, data := range map[string][]byte{
		"animated.webp": webpSample(t, webpAnimated),
		"cut.webp":      lossless[:25],
		"still.webp":    lossless,
	} {
		write(t, root, name, data)
		got := readFile(t, rd, &agentv1.ReadArgs{Path: name})
		require.NotNil(t, got.GetSuccess(), "%s: %v", name, got)
		assert.Equal(t, data, got.GetSuccess().GetData(), name)
	}
}
