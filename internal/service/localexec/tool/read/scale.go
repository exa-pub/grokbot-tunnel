package read

import (
	"compress/zlib"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"io"
)

// boxScaler scales an image down row by row, as the desktop's streaming PNG
// scaling does (Jlt): source column x goes to column x·w/sw, row y to row
// y·h/sh, and each target pixel averages its source pixels weighted by
// their alpha. It holds only the target image and one row of sums.
type boxScaler struct {
	dst    *image.NRGBA
	sh, y  int
	column []int
	sums   []uint64 // premultiplied red, green, blue and alpha by target column
	counts []uint64
}

func newBoxScaler(sw, sh, w, h int) *boxScaler {
	s := &boxScaler{
		dst:    image.NewNRGBA(image.Rect(0, 0, w, h)),
		sh:     sh,
		column: make([]int, sw),
		sums:   make([]uint64, 4*w),
		counts: make([]uint64, w),
	}
	for x := range s.column {
		s.column[x] = x * w / sw
	}
	return s
}

// add adds the pixel at column x of the current source row, in
// premultiplied 16-bit color.
func (s *boxScaler) add(x int, r, g, b, a uint32) {
	c := s.column[x]
	p := s.sums[4*c : 4*c+4 : 4*c+4]
	p[0] += uint64(r)
	p[1] += uint64(g)
	p[2] += uint64(b)
	p[3] += uint64(a)
	s.counts[c]++
}

// endRow ends a source row, and writes a target row after its last one.
func (s *boxScaler) endRow() {
	h := s.dst.Rect.Dy()
	row := s.y * h / s.sh
	s.y++
	if s.y == s.sh || s.y*h/s.sh != row {
		s.flush(row)
	}
}

func (s *boxScaler) flush(row int) {
	pix := s.dst.Pix[row*s.dst.Stride:]
	for c, n := range s.counts {
		p := s.sums[4*c : 4*c+4 : 4*c+4]
		if a := p[3]; a > 0 {
			for i := range 3 {
				pix[4*c+i] = uint8((p[i]*0xff + a/2) / a) //nolint:gosec // p[i] ≤ a
			}
			pix[4*c+3] = uint8((a + n*0x101/2) / (n * 0x101)) //nolint:gosec // a ≤ n·0xffff
		}
		clear(p)
		s.counts[c] = 0
	}
}

// scale scales img down to w×h.
func scale(img image.Image, w, h int) *image.NRGBA {
	b := img.Bounds()
	s := newBoxScaler(b.Dx(), b.Dy(), w, h)
	switch src := img.(type) {
	case *image.NRGBA:
		for y := b.Min.Y; y < b.Max.Y; y++ {
			row := src.Pix[src.PixOffset(b.Min.X, y):]
			for x := range b.Dx() {
				p := row[4*x : 4*x+4 : 4*x+4]
				a := uint32(p[3]) * 0x101
				s.add(x, uint32(p[0])*a/0xff, uint32(p[1])*a/0xff, uint32(p[2])*a/0xff, a)
			}
			s.endRow()
		}
	case image.RGBA64Image:
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				c := src.RGBA64At(x, y)
				s.add(x-b.Min.X, uint32(c.R), uint32(c.G), uint32(c.B), uint32(c.A))
			}
			s.endRow()
		}
	default:
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				r, g, bl, a := img.At(x, y).RGBA()
				s.add(x-b.Min.X, r, g, bl, a)
			}
			s.endRow()
		}
	}
	return s.dst
}

// onCanvas is an image on a canvas, transparent around it, as jimp decodes
// the first frame of a GIF.
type onCanvas struct {
	image.Image
	canvas image.Rectangle
}

func (c onCanvas) Bounds() image.Rectangle { return c.canvas }

func (c onCanvas) At(x, y int) color.Color { return c.RGBA64At(x, y) }

func (c onCanvas) RGBA64At(x, y int) color.RGBA64 {
	if !image.Pt(x, y).In(c.Image.Bounds()) {
		return color.RGBA64{}
	}
	r, g, b, a := c.Image.At(x, y).RGBA()
	return color.RGBA64{uint16(r), uint16(g), uint16(b), uint16(a)} //nolint:gosec // 16-bit values
}

// orient turns img by an EXIF Orientation as jimp does (Zdr, Xdr); 5–8
// swap its sides.
func orient(img *image.NRGBA, orientation int) *image.NRGBA {
	if orientation < 2 || orientation > 8 {
		return img
	}
	sw, sh := img.Rect.Dx(), img.Rect.Dy()
	w, h := sw, sh
	if orientation > 4 {
		w, h = sh, sw
	}
	// source returns the source pixel of target pixel (x, y).
	source := [9]func(x, y int) (int, int){
		2: func(x, y int) (int, int) { return sw - 1 - x, y },
		3: func(x, y int) (int, int) { return sw - 1 - x, sh - 1 - y },
		4: func(x, y int) (int, int) { return x, sh - 1 - y },
		5: func(x, y int) (int, int) { return y, x },
		6: func(x, y int) (int, int) { return y, sh - 1 - x },
		7: func(x, y int) (int, int) { return sw - 1 - y, sh - 1 - x },
		8: func(x, y int) (int, int) { return sw - 1 - y, x },
	}[orientation]
	out := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			sx, sy := source(x, y)
			copy(out.Pix[out.PixOffset(x, y):][:4], img.Pix[img.PixOffset(sx, sy):][:4])
		}
	}
	return out
}

// pngRows is an 8- or 16-bit PNG without palette, transparency chunk or
// interlacing, which can be scaled as it is inflated. The desktop scales
// 8-bit ones so (J_r, Jlt) and decodes 16-bit ones whole.
type pngRows struct {
	w, h, channels, depth int
	idat                  [][]byte
}

// streamablePNG returns the rows of a PNG that can be scaled as it is
// inflated (J_r).
func streamablePNG(b []byte) (pngRows, bool) {
	var p pngRows
	header := false
	for n := 8; n+12 <= len(b); {
		size := int(binary.BigEndian.Uint32(b[n:]))
		if size > len(b)-n-8 {
			return p, false
		}
		body := b[n+8 : n+8+size]
		switch string(b[n+4 : n+8]) {
		case "IHDR":
			if header || size != 13 {
				return p, false
			}
			channels := map[byte]int{0: 1, 2: 3, 4: 2, 6: 4}[body[9]]
			if body[8] != 8 && body[8] != 16 || body[12] != 0 || channels == 0 {
				return p, false
			}
			header = true
			p.w, p.h = int(binary.BigEndian.Uint32(body)), int(binary.BigEndian.Uint32(body[4:]))
			p.channels, p.depth = channels, int(body[8])
		case "IDAT":
			p.idat = append(p.idat, body)
		case "tRNS":
			return p, false
		case "IEND":
			return p, header && len(p.idat) > 0
		}
		n += size + 12
	}
	return p, header && len(p.idat) > 0
}

// scale inflates the rows and scales them down to w×h.
func (p pngRows) scale(ctx context.Context, w, h int) (*image.NRGBA, error) {
	zr, err := zlib.NewReader(io.MultiReader(readers(p.idat)...))
	if err != nil {
		return nil, err
	}
	bpp := p.channels * p.depth / 8
	cur, prev := make([]byte, 1+p.w*bpp), make([]byte, 1+p.w*bpp)
	s := newBoxScaler(p.w, p.h, w, h)
	for y := range p.h {
		if y%64 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if _, err = io.ReadFull(zr, cur); err != nil {
			return nil, err
		}
		if err = unfilter(cur[0], cur[1:], prev[1:], bpp); err != nil {
			return nil, err
		}
		p.add(s, cur[1:])
		s.endRow()
		cur, prev = prev, cur
	}
	return s.dst, nil
}

// add adds an unfiltered row to the scaler.
func (p pngRows) add(s *boxScaler, row []byte) {
	step := p.depth / 8
	sample := func(i int) uint32 {
		if step == 1 {
			return uint32(row[i]) * 0x101
		}
		return uint32(row[i])<<8 | uint32(row[i+1])
	}
	for x := range p.w {
		i := x * p.channels * step
		r := sample(i)
		g, b, a := r, r, uint32(0xffff)
		switch p.channels {
		case 2:
			a = sample(i + step)
		case 3:
			g, b = sample(i+step), sample(i+2*step)
		case 4:
			g, b, a = sample(i+step), sample(i+2*step), sample(i+3*step)
		}
		s.add(x, r*a/0xffff, g*a/0xffff, b*a/0xffff, a)
	}
}

// unfilter reverses a PNG row filter in place.
func unfilter(filter byte, row, prev []byte, bpp int) error {
	switch filter {
	case 0:
	case 1:
		for i := bpp; i < len(row); i++ {
			row[i] += row[i-bpp]
		}
	case 2:
		for i := range row {
			row[i] += prev[i]
		}
	case 3:
		for i := range row {
			var left byte
			if i >= bpp {
				left = row[i-bpp]
			}
			row[i] += left>>1 + prev[i]>>1 + left&prev[i]&1
		}
	case 4:
		for i := range row {
			var a, c byte
			if i >= bpp {
				a, c = row[i-bpp], prev[i-bpp]
			}
			row[i] += paeth(a, prev[i], c)
		}
	default:
		return fmt.Errorf("unknown PNG filter type %d", filter)
	}
	return nil
}

func paeth(a, b, c byte) byte {
	p := int(a) + int(b) - int(c)
	pa, pb, pc := abs(p-int(a)), abs(p-int(b)), abs(p-int(c))
	switch {
	case pa <= pb && pa <= pc:
		return a
	case pb <= pc:
		return b
	}
	return c
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
