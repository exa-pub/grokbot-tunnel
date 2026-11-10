package read

import (
	"bytes"
	"compress/zlib"
	"context"
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

	_ "golang.org/x/image/webp" // registers the WEBP decoder
)

// Limits of the desktop's image preparation (Hct, mg, Gct, Fct, Hs, oye).
const (
	maxImageBytes = 1 << 20 // mg
	maxImageSide  = 1024    // Gct
	maxWebPSide   = 1280    // Fct
	minImageSide  = 8       // Hs
	// maxImagePixels is the desktop's decode limit ($ct), lowered here; 50
	// Mpx fit an 8K screenshot.
	maxImagePixels  = 50_000_000
	maxImageSideLen = 1_000_000 // Jct
	// maxDecodeBytes bounds the memory of decoding an image, which a file
	// of a few hundred KB can make hundreds of MiB.
	maxDecodeBytes = 256 << 20
	// maxProgressiveJPEGBytes is maxDecodeBytes for a progressive JPEG, whose
	// coefficients Go keeps as int32: enough for every JPEG the desktop's
	// jpeg-js decodes within its maxMemoryUsageInMB of 512, up to 24.4 Mpx
	// at 4:4:4 (15 bytes a pixel here). Images are decoded one at a time.
	maxProgressiveJPEGBytes = 384 << 20
	jpegQuality             = 100 // jimp's default
	webpAsJPEG              = 90
)

// webpKeptSizes are WEBP sizes the desktop passes on without resizing (W3r).
var webpKeptSizes = []image.Point{{1280, 800}, {1456, 840}}

// decodeSlot lets one image at a time be decoded in the process, so that
// parallel reads do not multiply the memory decoding takes.
var decodeSlot = make(chan struct{}, 1)

const msgInvalidImage = "File is not a valid image: its data could not be decoded (%s). " +
	"A truncated or hand-copied file does this; write it again from the original bytes with code."

// msgWebPCodec is the desktop's error for a WEBP it would have to decode
// (tNe): it registers no WEBP codec.
const msgWebPCodec = "webp codec not registered: registerWebpCodec() must be called by a Node server before decoding/encoding webp"

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

// imageHeader is what an image's header tells before decoding.
type imageHeader struct {
	w, h int
	// orientation is a JPEG's EXIF Orientation, 1 when there is none.
	orientation int
	// target is the size to pass the image on at, turned by orientation.
	target image.Point
	resize bool
	// rows are those of a PNG that is scaled as it is inflated.
	rows *pngRows
}

// prepareImage prepares an image for the model like the desktop's tdt: an
// image within 1024 pixels (WEBP: 1280) and 1 MiB is passed on as it is once
// it decodes; a larger one is scaled down and encoded again in its format,
// smaller by a fifth until it fits 1 MiB. A JPEG is turned by its EXIF
// Orientation first, as jimp does.
//
// Images are decoded one at a time and within maxDecodeBytes; a PNG to
// scale down is scaled as it is inflated where it can be, as the desktop
// does with an 8-bit one.
//
// The desktop passes such a WEBP on by its header, without decoding: it
// registers no WEBP codec, and answers a larger one with an error
// (msgWebPCodec). Here a larger WEBP is decoded and encoded again as JPEG,
// or PNG when it has transparency.
func prepareImage(ctx context.Context, data []byte) ([]byte, error) {
	kind := kindOf(data)
	if kind == kindWebP && webpAsIs(data) {
		return data, nil
	}
	select {
	case decodeSlot <- struct{}{}:
		defer func() { <-decodeSlot }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	hdr, err := readImageHeader(data, kind)
	if err != nil {
		return nil, err
	}
	if !hdr.resize && len(data) <= maxImageBytes {
		if _, err = decode(data, kind, hdr); err != nil {
			return nil, err
		}
		return data, nil
	}
	src, err := pixels(ctx, data, kind, hdr)
	if err != nil {
		return nil, err
	}
	w, h := hdr.target.X, hdr.target.Y
	out, err := encodeImage(src, kind, hdr.orientation, w, h)
	for err == nil && len(out) > maxImageBytes && w > minImageSide && h > minImageSide {
		w, h = max(minImageSide, jsRound(float64(w)*0.8)), max(minImageSide, jsRound(float64(h)*0.8))
		out, err = encodeImage(src, kind, hdr.orientation, w, h)
	}
	return out, err
}

// webpAsIs reports a WEBP the desktop passes on as it is by its header
// (ayr): of a kept size or within 1280 pixels, and within 1 MiB.
func webpAsIs(b []byte) bool {
	size, ok := webpSize(b)
	if !ok || len(b) > maxImageBytes {
		return false
	}
	_, _, resize := fitImage(size.X, size.Y, maxWebPSide)
	return !resize || slices.Contains(webpKeptSizes, size)
}

// webpSize reads the size of a WEBP from its header, as the desktop's toe
// does.
func webpSize(b []byte) (image.Point, bool) {
	if len(b) < 16 || string(b[:4]) != "RIFF" || string(b[8:12]) != "WEBP" {
		return image.Point{}, false
	}
	switch string(b[12:16]) {
	case "VP8 ":
		if len(b) < 30 || b[23] != 0x9d || b[24] != 0x01 || b[25] != 0x2a {
			return image.Point{}, false
		}
		return image.Pt(int(binary.LittleEndian.Uint16(b[26:])&0x3fff), int(binary.LittleEndian.Uint16(b[28:])&0x3fff)), true
	case "VP8L":
		if len(b) < 25 || b[20] != 0x2f {
			return image.Point{}, false
		}
		v := binary.LittleEndian.Uint32(b[21:])
		return image.Pt(int(v&0x3fff)+1, int(v>>14&0x3fff)+1), true
	case "VP8X":
		if len(b) < 30 {
			return image.Point{}, false
		}
		return image.Pt(1+int(b[24])|int(b[25])<<8|int(b[26])<<16, 1+int(b[27])|int(b[28])<<8|int(b[29])<<16), true
	}
	return image.Point{}, false
}

// webpFrameSize reads the size of the first VP8 or VP8L frame of a WEBP,
// the one Go decodes.
func webpFrameSize(b []byte) (image.Point, bool) {
	for n := 12; n+8 <= len(b); {
		size := int(binary.LittleEndian.Uint32(b[n+4:]))
		body := b[n+8 : n+8+min(size, len(b)-n-8)]
		switch string(b[n : n+4]) {
		case "VP8 ":
			if len(body) < 10 || body[3] != 0x9d || body[4] != 0x01 || body[5] != 0x2a {
				return image.Point{}, false
			}
			return image.Pt(int(binary.LittleEndian.Uint16(body[6:])&0x3fff), int(binary.LittleEndian.Uint16(body[8:])&0x3fff)), true
		case "VP8L":
			if len(body) < 5 || body[0] != 0x2f {
				return image.Point{}, false
			}
			v := binary.LittleEndian.Uint32(body[1:])
			return image.Pt(int(v&0x3fff)+1, int(v>>14&0x3fff)+1), true
		}
		n += 8 + size + size&1
	}
	return image.Point{}, false
}

// readImageHeader checks an image before decoding as the desktop's qct
// does: its size must be readable and within limits, and a PNG must hold
// all its pixel data. An image that is decoded must also fit the memory
// budget, which is checked first.
func readImageHeader(data []byte, kind imageKind) (imageHeader, error) {
	hdr := imageHeader{orientation: 1}
	var err error
	if hdr.w, hdr.h, err = imageSize(data, kind); err != nil {
		return hdr, err
	}
	if err = checkDimensions(hdr.w, hdr.h); err != nil {
		return hdr, err
	}
	switch kind {
	case kindJPEG:
		hdr.orientation = readJPEG(data).orientation
	case kindWebP:
		// Go decodes the frame of an extended WEBP whatever its canvas;
		// one that is not the size of its canvas (or animated) gets the
		// desktop's error.
		if frame, ok := webpFrameSize(data); !ok || frame != image.Pt(hdr.w, hdr.h) {
			return hdr, errors.New(msgWebPCodec)
		}
	}
	hdr.target.X, hdr.target.Y, hdr.resize = hdr.fit(kind)
	if kind == kindPNG && hdr.resize {
		if rows, ok := streamablePNG(data); ok {
			hdr.rows = &rows
		}
	}
	if hdr.rows == nil {
		if err = checkDecodeBudget(data, kind, hdr); err != nil {
			return hdr, err
		}
	}
	if kind == kindPNG {
		if err = checkPNGPixelData(data); err != nil {
			return hdr, undecodable(err)
		}
	}
	return hdr, nil
}

// imageSize reads the size of an image from its header.
func imageSize(data []byte, kind imageKind) (int, int, error) {
	errSize := errors.New("image dimensions could not be read before decoding")
	if kind == kindWebP {
		size, ok := webpSize(data)
		if !ok {
			return 0, 0, errSize
		}
		return size.X, size.Y, nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	switch {
	case err != nil && kind == kindJPEG:
		return 0, 0, undecodable(err)
	case err != nil:
		return 0, 0, errSize
	}
	return cfg.Width, cfg.Height, nil
}

// fit returns the size to pass an image on at, its sides turned by its
// orientation.
func (hdr imageHeader) fit(kind imageKind) (int, int, bool) {
	w, h := hdr.w, hdr.h
	if hdr.orientation > 4 {
		w, h = h, w
	}
	switch {
	case kind == kindWebP && slices.Contains(webpKeptSizes, image.Pt(w, h)):
		return w, h, false
	case kind == kindWebP:
		return fitImage(w, h, maxWebPSide)
	}
	return fitImage(w, h, maxImageSide)
}

// pixels returns the pixels to scale: a PNG that can be is scaled to the
// target size as it is inflated, as the desktop does (Jlt); other images,
// and one whose rows do not inflate, are decoded.
func pixels(ctx context.Context, data []byte, kind imageKind, hdr imageHeader) (image.Image, error) {
	if hdr.rows != nil {
		img, err := hdr.rows.scale(ctx, hdr.target.X, hdr.target.Y)
		switch {
		case err == nil:
			return img, nil
		case ctx.Err() != nil:
			return nil, ctx.Err()
		}
	}
	return decode(data, kind, hdr)
}

// decode decodes an image whose decoding fits the memory budget.
func decode(data []byte, kind imageKind, hdr imageHeader) (image.Image, error) {
	if err := checkDecodeBudget(data, kind, hdr); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	switch {
	case err != nil && kind == kindWebP:
		return nil, errors.New(msgWebPCodec)
	case err != nil:
		return nil, undecodable(err)
	}
	if canvas := image.Rect(0, 0, hdr.w, hdr.h); img.Bounds() != canvas {
		// A GIF's first frame, which may cover part of its canvas.
		return onCanvas{img, canvas}, nil
	}
	return img, nil
}

// checkDecodeBudget refuses an image whose decoding would take more than
// its decodeBudget, in the words of the pixel limit for its pixel format.
func checkDecodeBudget(data []byte, kind imageKind, hdr imageHeader) error {
	n, budget := decodeBytes(data, kind, hdr.w, hdr.h), decodeBudget(data, kind)
	if n <= budget {
		return nil
	}
	px := int64(hdr.w) * int64(hdr.h)
	return fmt.Errorf("image dimensions %dx%d exceed the %d pixel decode limit", hdr.w, hdr.h, budget*px/n)
}

// decodeBudget is the memory decoding an image of kind may take.
func decodeBudget(data []byte, kind imageKind) int64 {
	if kind == kindJPEG && readJPEG(data).progressive {
		return maxProgressiveJPEGBytes
	}
	return maxDecodeBytes
}

// decodeBytes estimates the memory Go's decoder takes for a w×h image: the
// pixels of the image it returns, with those of the interlacing passes of
// a PNG and the coefficients of a progressive JPEG.
func decodeBytes(data []byte, kind imageKind, w, h int) int64 {
	px := float64(w) * float64(h)
	perPixel := 1.0 // GIF: a paletted frame
	switch kind {
	case kindPNG:
		perPixel = pngPixelSize(data)
	case kindJPEG:
		perPixel = readJPEG(data).pixelSize()
	case kindWebP:
		perPixel = 10 // VP8L, alpha: pixels decoded as NRGBA, then copied
		if len(data) >= 16 && string(data[12:16]) == "VP8 " {
			perPixel = 2 // YCbCr 4:2:0
		}
	}
	return int64(math.Ceil(px * perPixel))
}

// pngPixelSize is the bytes a pixel of a PNG takes once decoded.
func pngPixelSize(b []byte) float64 {
	if len(b) < 29 {
		return 8
	}
	depth, colorType, interlaced := b[24], b[25], b[28] == 1
	transparent := false
	for n := 8; n+8 <= len(b) && string(b[n+4:n+8]) != "IDAT"; n += int(binary.BigEndian.Uint32(b[n:])) + 12 {
		transparent = transparent || string(b[n+4:n+8]) == "tRNS"
	}
	size := 4.0 // RGBA, NRGBA
	switch {
	case colorType == 3:
		size = 1 // Paletted
	case colorType == 0 && !transparent:
		size = 1 // Gray
		if depth == 16 {
			size = 2
		}
	case depth == 16:
		size = 8
	}
	if interlaced {
		size *= 2
	}
	return size
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

// encodeImage scales img to w×h, turned by orientation, and encodes it.
func encodeImage(img image.Image, kind imageKind, orientation, w, h int) ([]byte, error) {
	sw, sh := w, h
	if orientation > 4 {
		sw, sh = h, w
	}
	dst := orient(scale(img, sw, sh), orientation)
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
// (Y_r), or -1 for a header the check does not know.
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
