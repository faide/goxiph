// Package flac implements the Free Lossless Audio Codec defined by RFC 9639.
//
// Samples are int32 rather than the float32 of audio.Buffer. FLAC permits depths up to 32 bits,
// which a 24-bit float32 mantissa cannot hold, and a lossless codec that cannot represent its own
// input is a contradiction. Conversion to audio.Buffer is offered at the edges, where the depth
// limit is visible.
package flac

// crc8Table covers the frame header checksum, polynomial x^8 + x^2 + x + 1.
var crc8Table = buildCRC8()

// crc16Table covers the whole frame, polynomial x^16 + x^15 + x^2 + 1.
var crc16Table = buildCRC16()

func buildCRC8() *[256]uint8 {
	var t [256]uint8
	for i := range t {
		c := uint8(i)
		for range 8 {
			if c&0x80 != 0 {
				c = c<<1 ^ 0x07
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return &t
}

func buildCRC16() *[256]uint16 {
	var t [256]uint16
	for i := range t {
		c := uint16(i) << 8
		for range 8 {
			if c&0x8000 != 0 {
				c = c<<1 ^ 0x8005
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return &t
}

// crc8 checksums the frame header, which is always a whole number of bytes.
func crc8(p []byte) uint8 {
	var c uint8
	for _, b := range p {
		c = crc8Table[c^b]
	}
	return c
}

// crc16 checksums a complete frame including its sync code but excluding the checksum itself.
func crc16(p []byte) uint16 {
	var c uint16
	for _, b := range p {
		c = c<<8 ^ crc16Table[byte(c>>8)^b]
	}
	return c
}
