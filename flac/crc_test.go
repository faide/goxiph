package flac

import (
	"math/rand/v2"
	"testing"
)

// TestCRCSelfCheckProperty pins both checksums without needing an external vector.
//
// Both are initialised to zero with no final inversion, so appending the checksum to its own message
// leaves a remainder of zero. A wrong polynomial or bit direction breaks this immediately.
func TestCRCSelfCheckProperty(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 13))

	for range 200 {
		msg := make([]byte, rng.IntN(200)+1)
		for i := range msg {
			msg[i] = byte(rng.UintN(256))
		}

		c8 := crc8(msg)
		if got := crc8(append(append([]byte(nil), msg...), c8)); got != 0 {
			t.Fatalf("crc8 of message plus its checksum = %#02x, want 0", got)
		}

		c16 := crc16(msg)
		withCRC := append(append([]byte(nil), msg...), byte(c16>>8), byte(c16))
		if got := crc16(withCRC); got != 0 {
			t.Fatalf("crc16 of message plus its checksum = %#04x, want 0", got)
		}
	}
}

// TestCRCDetectsSingleBitFlips is the property a checksum exists for.
func TestCRCDetectsSingleBitFlips(t *testing.T) {
	msg := []byte("a frame header and some residual data")
	c8, c16 := crc8(msg), crc16(msg)

	for i := range msg {
		for bit := range 8 {
			flipped := append([]byte(nil), msg...)
			flipped[i] ^= 1 << bit
			if crc8(flipped) == c8 {
				t.Errorf("crc8 missed a flip of byte %d bit %d", i, bit)
			}
			if crc16(flipped) == c16 {
				t.Errorf("crc16 missed a flip of byte %d bit %d", i, bit)
			}
		}
	}
}

func TestCRCOfEmpty(t *testing.T) {
	if got := crc8(nil); got != 0 {
		t.Errorf("crc8(nil) = %#02x, want 0", got)
	}
	if got := crc16(nil); got != 0 {
		t.Errorf("crc16(nil) = %#04x, want 0", got)
	}
}
