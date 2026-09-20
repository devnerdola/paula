package media

import "encoding/binary"

// orientation reads the EXIF orientation of a JPEG, and is 0 when the image
// carries none. Photos from a phone are upright only once it is applied.
func orientation(b []byte) int {
	exif := app1(b)
	if exif == nil {
		return 0
	}
	var order binary.ByteOrder
	switch {
	case len(exif) < 8:
		return 0
	case string(exif[:2]) == "II":
		order = binary.LittleEndian
	case string(exif[:2]) == "MM":
		order = binary.BigEndian
	default:
		return 0
	}
	if order.Uint16(exif[2:]) != 42 {
		return 0
	}
	off := int(order.Uint32(exif[4:]))
	if off < 8 || off+2 > len(exif) {
		return 0
	}
	count := int(order.Uint16(exif[off:]))
	for i := range count {
		e := off + 2 + i*12
		if e+12 > len(exif) {
			return 0
		}
		if order.Uint16(exif[e:]) != 0x0112 {
			continue
		}
		// The orientation is one short; a tag of any other type is left alone.
		if order.Uint16(exif[e+2:]) != 3 || order.Uint32(exif[e+4:]) != 1 {
			return 0
		}
		return int(order.Uint16(exif[e+8:]))
	}
	return 0
}

// app1 returns the TIFF header of the Exif segment of a JPEG.
func app1(b []byte) []byte {
	if len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 {
		return nil
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xff {
			return nil
		}
		// A marker may be padded with fill bytes, which the standard allows.
		for i+1 < len(b) && b[i+1] == 0xff {
			i++
		}
		if i+4 > len(b) {
			return nil
		}
		marker := b[i+1]
		if marker == 0xd8 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			i += 2
			continue
		}
		// Start of scan: the entropy-coded data follows, with no more headers.
		if marker == 0xda || marker == 0xd9 {
			return nil
		}
		size := int(binary.BigEndian.Uint16(b[i+2:]))
		if size < 2 || i+2+size > len(b) {
			return nil
		}
		if marker == 0xe1 {
			seg := b[i+4 : i+2+size]
			if len(seg) > 6 && string(seg[:6]) == "Exif\x00\x00" {
				return seg[6:]
			}
		}
		i += 2 + size
	}
	return nil
}
