package read

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pngChunk(name string, body []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.BigEndian, uint32(len(body))) //nolint:gosec // test chunks are small
	b.WriteString(name)
	b.Write(body)
	_ = binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte(name), body...)))
	return b.Bytes()
}

// bombPNG is a w×h PNG of zeros: a few hundred KB on disk for hundreds of
// MiB of pixels. Interlaced, it holds the rows of the seven passes.
func bombPNG(t testing.TB, w, h int, depth, colorType byte, interlaced bool, extra ...[]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr, uint32(w)) //nolint:gosec // test sizes
	binary.BigEndian.PutUint32(ihdr[4:], uint32(h))
	ihdr[8], ihdr[9] = depth, colorType
	if interlaced {
		ihdr[12] = 1
	}
	out.Write(pngChunk("IHDR", ihdr))
	for _, c := range extra {
		out.Write(c)
	}
	var z bytes.Buffer
	zw, err := zlib.NewWriterLevel(&z, zlib.BestCompression)
	require.NoError(t, err)
	zeros := make([]byte, 1<<20)
	for n := pngPixelBytes(ihdr); n > 0; n -= int64(len(zeros)) {
		_, err = zw.Write(zeros[:min(n, int64(len(zeros)))])
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	out.Write(pngChunk("IDAT", z.Bytes()))
	out.Write(pngChunk("IEND", nil))
	return out.Bytes()
}

// allocated returns the bytes f allocates.
func allocated(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// peakHeap returns the largest heap seen while f runs.
func peakHeap(f func()) uint64 {
	runtime.GC()
	var peak uint64
	done, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			peak = max(peak, m.HeapAlloc)
			select {
			case <-done:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	f()
	close(done)
	<-stopped
	return peak
}

// A PNG scaled as it is inflated comes out as the decoded one scaled, for
// each color type and depth that is streamed and each row filter.
func TestPNGRowsScaleLikeDecoded(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // test pixels
	w, h := 301, 157
	gray, gray16 := image.NewGray(image.Rect(0, 0, w, h)), image.NewGray16(image.Rect(0, 0, w, h))
	nrgba, nrgba64 := image.NewNRGBA(image.Rect(0, 0, w, h)), image.NewNRGBA64(image.Rect(0, 0, w, h))
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			v := uint8(r.UintN(256)) //nolint:gosec // < 256
			if y%3 == 0 {
				v = uint8(x + y) //nolint:gosec // smooth rows get other filters
			}
			c := color.NRGBA64{uint16(v) * 257, uint16(255-v) * 257, uint16(x) * 211, uint16(r.UintN(65536))} //nolint:gosec // < 65536
			gray.SetGray(x, y, color.Gray{v})
			gray16.SetGray16(x, y, color.Gray16{c.R ^ c.A})
			nrgba.Set(x, y, c)
			nrgba64.SetNRGBA64(x, y, c)
			rgba.SetRGBA(x, y, color.RGBA{v, 255 - v, uint8(x), 255}) //nolint:gosec // wraps on purpose
		}
	}
	for _, img := range []image.Image{gray, gray16, nrgba, nrgba64, rgba} {
		var b bytes.Buffer
		require.NoError(t, png.Encode(&b, img))
		rows, ok := streamablePNG(b.Bytes())
		require.True(t, ok, "%T", img)
		decoded, err := png.Decode(bytes.NewReader(b.Bytes()))
		require.NoError(t, err)
		for _, size := range []image.Point{{w, h}, {100, 52}, {37, 8}} {
			got, err := rows.scale(context.Background(), size.X, size.Y)
			require.NoError(t, err)
			assert.Equal(t, scale(decoded, size.X, size.Y).Pix, got.Pix, "%T to %v", img, size)
		}
	}
}

// A 7000×7000 PNG of 190–380 KB is scaled as it is inflated, as the
// desktop's Jlt does, instead of being decoded into 200–400 MiB of pixels
// (F2-3).
func TestImageBombIsStreamed(t *testing.T) {
	if raceEnabled {
		t.Skip("memory measurement")
	}
	for _, depth := range []byte{8, 16} {
		data := bombPNG(t, 7000, 7000, depth, 6, false)
		var out []byte
		var err error
		alloc := allocated(func() { out, err = prepareImage(context.Background(), data) })
		require.NoError(t, err, depth)
		cfg, format, err := image.DecodeConfig(bytes.NewReader(out))
		require.NoError(t, err)
		assert.Equal(t, "png", format)
		assert.Equal(t, image.Pt(1024, 1024), image.Pt(cfg.Width, cfg.Height))
		t.Logf("%d-bit: %d KB on disk, %d MiB allocated", depth, len(data)>>10, alloc>>20)
		assert.Less(t, alloc, uint64(32<<20), "%d-bit", depth)
	}
}

// An image that is not scaled as it is inflated is decoded only within a
// memory budget; a larger one is refused like one with too many pixels,
// before its data is inflated (F2-3).
func TestImageDecodeBudget(t *testing.T) {
	ihdr := []byte{0, 0, 0x1b, 0x58, 0, 0, 0x1b, 0x58, 16, 6, 0, 0, 1} // 7000×7000, interlaced 16-bit RGBA
	data := slices.Concat([]byte("\x89PNG\r\n\x1a\n"), pngChunk("IHDR", ihdr), pngChunk("IDAT", []byte{0x78, 0x9c}), pngChunk("IEND", nil))
	_, err := prepareImage(context.Background(), data)
	require.EqualError(t, err, "image dimensions 7000x7000 exceed the 16777216 pixel decode limit", "8 bytes a pixel, twice for the passes")

	for _, tc := range []struct {
		name  string
		data  []byte
		bytes int64
	}{
		{"gray", bombPNG(t, 400, 300, 8, 0, false), 400 * 300},
		{"gray16", bombPNG(t, 400, 300, 16, 0, false), 400 * 300 * 2},
		{"rgb with tRNS", bombPNG(t, 400, 300, 8, 2, false, pngChunk("tRNS", make([]byte, 6))), 400 * 300 * 4},
		{"rgba16", bombPNG(t, 400, 300, 16, 6, false), 400 * 300 * 8},
	} {
		assert.Equal(t, tc.bytes, decodeBytes(tc.data, kindPNG, 400, 300), tc.name)
	}
	var j bytes.Buffer
	require.NoError(t, jpeg.Encode(&j, image.NewYCbCr(image.Rect(0, 0, 400, 300), image.YCbCrSubsampleRatio420), nil))
	assert.Equal(t, int64(400*300*3/2), decodeBytes(j.Bytes(), kindJPEG, 400, 300), "YCbCr 4:2:0")
}

// Images are decoded one at a time, and a request waiting for its turn
// ends with its context (F2-3).
func TestImageDecodeOneAtATime(t *testing.T) {
	data := bombPNG(t, 1500, 1500, 8, 2, true) // not streamed: interlaced
	decodeSlot <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := prepareImage(ctx, data)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("decoded while another decode runs: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	<-decodeSlot

	if raceEnabled {
		return
	}
	// Eight parallel reads of a large image take the memory of about one.
	big := bombPNG(t, 3000, 3000, 8, 2, true) // 36 MiB decoded, 18 MiB for a pass
	one := peakHeap(func() {
		_, err := prepareImage(context.Background(), big)
		assert.NoError(t, err)
	})
	var wg sync.WaitGroup
	eight := peakHeap(func() {
		for range 8 {
			wg.Go(func() {
				_, err := prepareImage(context.Background(), big)
				assert.NoError(t, err)
			})
		}
		wg.Wait()
	})
	t.Logf("peak heap: %d MiB for one read, %d MiB for eight", one>>20, eight>>20)
	assert.Less(t, eight, 4*one, "peak heap: %d MiB for one read, %d MiB for eight", one>>20, eight>>20)
}

// sofJPEG is the start of a JPEG with a frame of w×h whose components have
// the sampling factors hv, progressive or not: enough for the budget.
func sofJPEG(w, h int, progressive bool, hv ...byte) []byte {
	marker := byte(0xc0)
	if progressive {
		marker = 0xc2
	}
	n := 8 + 3*len(hv)
	b := []byte{0xff, 0xd8, 0xff, marker, byte(n >> 8), byte(n), 8, byte(h >> 8), byte(h), byte(w >> 8), byte(w), byte(len(hv))}
	for i, f := range hv {
		b = append(b, byte(i+1), f, 0)
	}
	return append(b, 0xff, 0xd9)
}

// A progressive JPEG gets a larger budget: every one the desktop's jpeg-js
// decodes within its 512 MiB fits, up to 24.4 Mpx at 4:4:4 (F3-1). The
// limits of jpeg-js were measured by running it from the desktop's bundle:
// 24.38 Mpx at 4:4:4, 36.95 at 4:2:0, 53.67 for gray, progressive or not.
func TestImageDecodeBudgetProgressiveJPEG(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		ok   bool
	}{
		{"progressive 4:4:4 24 Mpx", sofJPEG(6000, 4000, true, 0x11, 0x11, 0x11), true},
		{"progressive 4:4:4 24.4 Mpx", sofJPEG(6048, 4032, true, 0x11, 0x11, 0x11), true},
		{"progressive 4:2:0 50 Mpx", sofJPEG(8660, 5770, true, 0x22, 0x11, 0x11), true},
		{"progressive gray 50 Mpx", sofJPEG(8660, 5770, true, 0x11), true},
		{"progressive 4:4:4 28 Mpx", sofJPEG(7000, 4000, true, 0x11, 0x11, 0x11), false},
		{"baseline 4:4:4 50 Mpx", sofJPEG(8660, 5770, false, 0x11, 0x11, 0x11), true},
	} {
		hdr := imageHeader{w: int(binary.BigEndian.Uint16(tc.data[9:])), h: int(binary.BigEndian.Uint16(tc.data[7:]))}
		err := checkDecodeBudget(tc.data, kindJPEG, hdr)
		if tc.ok {
			assert.NoError(t, err, tc.name)
		} else {
			assert.ErrorContains(t, err, "pixel decode limit", tc.name)
		}
	}
}
