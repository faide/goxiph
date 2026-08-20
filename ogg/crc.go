// Package ogg implements the Ogg container defined by RFC 3533.
package ogg

// crcPoly is the Ogg framing polynomial, RFC 3533 section 6.
//
// This is not a standard CRC-32: init 0, no input reflection, no output reflection, no final XOR.
// hash/crc32 reflects and cannot produce it.
const crcPoly = 0x04c11db7

var crcTable = buildCRCTable()

func buildCRCTable() *[256]uint32 {
	var t [256]uint32
	for i := range t {
		r := uint32(i) << 24
		for range 8 {
			if r&0x80000000 != 0 {
				r = r<<1 ^ crcPoly
			} else {
				r <<= 1
			}
		}
		t[i] = r
	}
	return &t
}

// crcUpdate folds p into the running checksum.
func crcUpdate(crc uint32, p []byte) uint32 {
	for _, b := range p {
		crc = crc<<8 ^ crcTable[byte(crc>>24)^b]
	}
	return crc
}

// crcOf computes the checksum of a complete page image whose CRC field is already zeroed.
func crcOf(page []byte) uint32 {
	return crcUpdate(0, page)
}
