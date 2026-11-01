package read

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"slices"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // registers the WEBP decoder
)

// Limits of the desktop's image preparation (Hct, mg, Gct, Fct, Hs, oye).
const (
	maxImageBytes = 1 << 20 // mg
	maxImageSide  = 1024    // Gct
	maxWebPSide   = 1280    // Fct
	minImageSide  = 8       // Hs
	// maxImagePixels is the desktop's decode limit ($ct), lowered here so
	// that decoding a hostile image cannot take gigabytes; 50 Mpx fit an 8K
	// screenshot.
	maxImagePixels  = 50_000_000
	maxImageSideLen = 1_000_000 // Jct
	jpegQuality     = 100       // jimp's default
	webpAsJPEG      = 90
)

// webpKeptSizes are WEBP sizes the desktop passes on without resizing (W3r).
var webpKeptSizes = []image.Point{{1280, 800}, {1456, 840}}

const msgInvalidImage = "File is not a valid image: its data could not be decoded (%s). " +
	"A truncated or hand-copied file does this; write it again from the original bytes with code."

// undecodableError is an image whose data does not decode; the agent sees it
// as an invalid file (Vct).
type undecodableError struct{ err error }

func (e *undecodableError) Error() string  { return e.err.Error() }
func (e *undecodableError) Unwrap() error  { return e.err }
func (e *undecodableError) Reason() string { return fmt.Sprintf(msgInvalidImage, e.err.Error()) }

func undecodable(err error) error { return &undecodableError{err: err} }

type imageKind int

const (
	kindPNG imageKind = iota
	kindJPEG
	kindGIF
	kindWebP
)

func kindOf(b []byte) imageKind {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG")):
		return kindPNG
	case bytes.HasPrefix(b, []byte("RIFF")):
		return kindWebP
	case bytes.HasPrefix(b, []byte("GIF")):
		return kindGIF
	}
	return kindJPEG
}

// prepareImage prepares an image for the model like the desktop's Wct: an
// image within 1024 pixels (WEBP: 1280) and 1 MiB is passed on as it is once
// it decodes; a larger one is scaled down and encoded again in its format,
// smaller by a fifth until it fits 1 MiB. Go has no WEBP encoder: a WEBP
// that needs it is encoded as JPEG, or PNG when it has transparency.
func prepareImage(data []byte) ([]byte, error) {
	kind := kindOf(data)
	img, cfg, err := decodeImage(data, kind)
	if err != nil {
		return nil, err
	}
	side := maxImageSide
	if kind == kindWebP {
		side = maxWebPSide
	}
	w, h, resize := fitImage(cfg.Width, cfg.Height, side)
	if kind == kindWebP && slices.Contains(webpKeptSizes, image.Pt(cfg.Width, cfg.Height)) {
		w, h, resize = cfg.Width, cfg.Height, false
	}
	if !resize && len(data) <= maxImageBytes {
		return data, nil
	}
	out, err := encodeImage(img, kind, w, h)
	for err == nil && len(out) > maxImageBytes && w > minImageSide && h > minImageSide {
		w, h = max(minImageSide, jsRound(float64(w)*0.8)), max(minImageSide, jsRound(float64(h)*0.8))
		out, err = encodeImage(img, kind, w, h)
	}
	return out, err
}

// decodeImage checks and decodes an image as the desktop's qct does: its
// size must be readable and within limits, a PNG must hold all its pixel
// data, and the data must decode.
func decodeImage(data []byte, kind imageKind) (image.Image, image.Config, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	switch {
	case err != nil && kind == kindJPEG:
		return nil, cfg, undecodable(err)
	case err != nil:
		return nil, cfg, errors.New("image dimensions could not be read before decoding")
	}
	if err = checkDimensions(cfg.Width, cfg.Height); err != nil {
		return nil, cfg, err
	}
	if kind == kindPNG {
		if err = checkPNGPixelData(data); err != nil {
			return nil, cfg, undecodable(err)
		}
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, cfg, undecodable(err)
	}
	return img, cfg, nil
}

// checkDimensions refuses images too large to decode (oye).
func checkDimensions(w, h int) error {
	if w*h > maxImagePixels {
		return fmt.Errorf("image dimensions %dx%d exceed the %d pixel decode limit", w, h, maxImagePixels)
	}
	if max(w, h) > maxImageSideLen {
		return fmt.Errorf("image dimensions %dx%d exceed the %d pixel side limit", w, h, maxImageSideLen)
	}
	return nil
}

func jsRound(x float64) int { return int(math.Floor(x + 0.5)) }

// fitImage scales w×h to fit side pixels, keeping both sides at least 8
// pixels (zct).
func fitImage(w, h, side int) (int, int, bool) {
	if w < 1 || h < 1 || max(w, h) <= side || min(w, h) < minImageSide {
		return w, h, false
	}
	scale := float64(side) / float64(max(w, h))
	nw, nh := max(1, jsRound(float64(w)*scale)), max(1, jsRound(float64(h)*scale))
	if nw < minImageSide || nh < minImageSide {
		scale = float64(minImageSide) / float64(min(w, h))
		nw, nh = max(minImageSide, jsRound(float64(w)*scale)), max(minImageSide, jsRound(float64(h)*scale))
	}
	return nw, nh, nw != w || nh != h
}

func encodeImage(img image.Image, kind imageKind, w, h int) ([]byte, error) {
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.BiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
	var out bytes.Buffer
	var err error
	switch {
	case kind == kindPNG, kind == kindWebP && !dst.Opaque():
		err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, dst)
	case kind == kindGIF:
		err = gif.Encode(&out, dst, nil)
	case kind == kindWebP:
		err = jpeg.Encode(&out, dst, &jpeg.Options{Quality: webpAsJPEG})
	default:
		err = jpeg.Encode(&out, dst, &jpeg.Options{Quality: jpegQuality})
	}
	return out.Bytes(), err
}

// checkPNGPixelData checks that a PNG holds all the pixel data its header
// promises, as the desktop does before decoding (sye); its texts are Node's
// zlib errors.
func checkPNGPixelData(b []byte) error {
	var idat [][]byte
	var want int64
	header := false
	for n := 8; n+12 <= len(b); {
		size := int(binary.BigEndian.Uint32(b[n:]))
		body := b[n+8 : min(len(b), n+8+size)]
		switch string(b[n+4 : n+8]) {
		case "IHDR":
			if header || len(body) != 13 {
				break
			}
			header = true
			if want = pngPixelBytes(body); want < 0 {
				return nil
			}
		case "IDAT":
			idat = append(idat, body)
		}
		if string(b[n+4:n+8]) == "IEND" || size > len(b) {
			break
		}
		n += size + 12
	}
	if !header || len(idat) == 0 {
		return nil
	}
	zr, err := zlib.NewReader(io.MultiReader(readers(idat)...))
	if err != nil {
		return zlibError(err)
	}
	n, err := io.CopyN(io.Discard, zr, want)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, io.EOF):
		return fmt.Errorf("PNG pixel data ends after %d of %d bytes", n, want)
	}
	return zlibError(err)
}

func readers(chunks [][]byte) []io.Reader {
	out := make([]io.Reader, len(chunks))
	for i, c := range chunks {
		out[i] = bytes.NewReader(c)
	}
	return out
}

// zlibError words a zlib failure as Node's zlib does where they match.
func zlibError(err error) error {
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return errors.New("unexpected end of file")
	case errors.Is(err, zlib.ErrHeader):
		return errors.New("incorrect header check")
	case errors.Is(err, zlib.ErrChecksum):
		return errors.New("incorrect data check")
	}
	return err
}

// pngPixelBytes is the size of the decompressed pixel data of a PNG header
// (F3r), or -1 for a header the check does not know.
func pngPixelBytes(ihdr []byte) int64 {
	channels := map[byte]int64{0: 1, 2: 3, 3: 1, 4: 2, 6: 4}[ihdr[9]]
	depth := int64(ihdr[8])
	if channels == 0 || !slices.Contains([]int64{1, 2, 4, 8, 16}, depth) || ihdr[12] > 1 {
		return -1
	}
	w, h := int64(binary.BigEndian.Uint32(ihdr)), int64(binary.BigEndian.Uint32(ihdr[4:]))
	bits := channels * depth
	rows := func(w, h int64) int64 {
		if w <= 0 || h <= 0 {
			return 0
		}
		return h * (1 + (w*bits+7)/8)
	}
	if ihdr[12] == 0 {
		return rows(w, h)
	}
	total := int64(0)
	for _, p := range [][4]int64{{0, 0, 8, 8}, {4, 0, 8, 8}, {0, 4, 4, 8}, {2, 0, 4, 4}, {0, 2, 2, 4}, {1, 0, 2, 2}, {0, 1, 1, 2}} {
		total += rows((w-p[0]+p[2]-1)/p[2], (h-p[1]+p[3]-1)/p[3])
	}
	return total
}
