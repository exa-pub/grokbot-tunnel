package read

import (
	"encoding/binary"
)

// jpegInfo is what the segments of a JPEG before its scan tell.
type jpegInfo struct {
	// orientation is the EXIF Orientation that jimp applies, 1 when it
	// applies none.
	orientation int
	// exif is the first Orientation found, 0 before one is.
	exif int
	// The frame: its components, their samples per pixel, progressive
	// coding, and RGB rather than YCbCr color.
	components  int
	samples     float64
	progressive bool
	rgb         bool
}

// readJPEG reads the segments of a JPEG up to its scan. They are walked as
// jimp's exif-parser walks them (parseSections): a segment that does not
// start with 0xff ends the walk and drops the EXIF.
func readJPEG(b []byte) jpegInfo {
	info := jpegInfo{orientation: 1}
	for n := 0; n < len(b); {
		if b[n] != 0xff || n+2 > len(b) {
			info.exif = 0
			break
		}
		marker := b[n+1]
		if marker >= 0xd0 && marker <= 0xd9 { // RST, SOI, EOI
			n += 2
			continue
		}
		if marker == 0xda { // SOS
			break
		}
		if n+4 > len(b) {
			info.exif = 0
			break
		}
		size := int(binary.BigEndian.Uint16(b[n+2:]))
		info.segment(marker, b[n+4:min(len(b), max(n+4, n+2+size))])
		n += 2 + size
	}
	if info.exif >= 2 && info.exif <= 8 {
		info.orientation = info.exif
	}
	return info
}

// segment reads a segment with a body.
func (info *jpegInfo) segment(marker byte, body []byte) {
	switch marker {
	case 0xe1: // APP1: the first Orientation wins
		if info.exif == 0 {
			info.exif = exifOrientation(body)
		}
	case 0xee: // APP14 Adobe, transform 0: RGB
		info.rgb = info.rgb || len(body) >= 12 && string(body[:5]) == "Adobe" && body[11] == 0
	case 0xc0, 0xc1, 0xc2: // the frames Go decodes
		info.frame(body, marker == 0xc2)
	}
}

// frame reads a SOF segment.
func (info *jpegInfo) frame(body []byte, progressive bool) {
	if len(body) < 6 || len(body) < 6+3*int(body[5]) {
		return
	}
	info.components, info.progressive = int(body[5]), progressive
	comps := body[6 : 6+3*info.components]
	var hmax, vmax, sum int
	for i := 0; i < len(comps); i += 3 {
		h, v := int(comps[i+1]>>4), int(comps[i+1]&0xf)
		hmax, vmax, sum = max(hmax, h), max(vmax, v), sum+h*v
	}
	switch {
	case info.components == 1 || hmax == 0 || vmax == 0:
		info.samples = float64(info.components)
	default:
		info.samples = float64(sum) / float64(hmax*vmax)
	}
	info.rgb = info.rgb || info.components == 3 && string([]byte{comps[0], comps[3], comps[6]}) == "RGB"
}

// pixelSize is the bytes a pixel takes while Go decodes the JPEG: its
// samples, the coefficients of a progressive one (an int32 a sample), and
// the RGBA or CMYK image Go converts RGB and four-component ones to.
func (info jpegInfo) pixelSize() float64 {
	if info.components == 0 {
		return 3 + 4*3 + 4 // unknown: progressive 4:4:4, converted
	}
	size := info.samples
	if info.progressive {
		size += 4 * info.samples
	}
	if info.components == 4 || info.components == 3 && info.rgb {
		size += 4
	}
	return size
}

// exifOrientation reads Orientation (0x0112) from IFD0 of an APP1 Exif
// segment, in either byte order, or returns 0. Like exif-parser it reads a
// single value of an integer type.
func exifOrientation(seg []byte) int {
	if len(seg) < 14 || string(seg[:6]) != "Exif\x00\x00" {
		return 0
	}
	tiff := seg[6:]
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 0
	}
	ifd := uint64(order.Uint32(tiff[4:]))
	if order.Uint16(tiff[2:]) != 42 || ifd+2 > uint64(len(tiff)) {
		return 0
	}
	entries := tiff[ifd+2:]
	for range order.Uint16(tiff[ifd:]) {
		if len(entries) < 12 {
			return 0
		}
		e := entries[:12]
		entries = entries[12:]
		if order.Uint16(e) != 0x0112 {
			continue
		}
		if order.Uint32(e[4:]) != 1 {
			return 0
		}
		var v uint32
		switch order.Uint16(e[2:]) {
		case 1, 6: // BYTE, SBYTE
			v = uint32(e[8])
		case 3, 8: // SHORT, SSHORT
			v = uint32(order.Uint16(e[8:]))
		case 4, 9: // LONG, SLONG
			v = order.Uint32(e[8:])
		}
		return int(min(v, 9))
	}
	return 0
}
