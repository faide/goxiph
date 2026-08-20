package flac

import (
	"bytes"
	"testing"

	"github.com/faide/goxiph/internal/bitio"
)

// FuzzDecodeAll drives the whole reader with arbitrary bytes. Malformed input must come back as an
// error rather than a panic, an unbounded allocation or a hang.
func FuzzDecodeAll(f *testing.F) {
	f.Add([]byte(Signature))
	f.Add(append([]byte(Signature), 0x80, 0, 0, 34))
	f.Add([]byte{0xFF, 0xF8})

	f.Fuzz(func(t *testing.T, data []byte) {
		samples, info, err := DecodeAll(bytes.NewReader(data))
		if err != nil {
			return
		}
		// Anything that decoded must be self-consistent, since callers will trust it.
		if len(samples) != info.Channels {
			t.Fatalf("decoded %d channels, stream info declares %d", len(samples), info.Channels)
		}
		for i := 1; i < len(samples); i++ {
			if len(samples[i]) != len(samples[0]) {
				t.Fatalf("channel %d has %d samples, channel 0 has %d",
					i, len(samples[i]), len(samples[0]))
			}
		}
	})
}

// FuzzReadSubframe reaches the subframe decoder directly, without the metadata and framing layers in
// front of it filtering the input.
func FuzzReadSubframe(f *testing.F) {
	b := newSubframeBuilderRaw()
	f.Add(b, uint8(16), uint16(64))
	f.Add([]byte{0x00}, uint8(8), uint16(4))

	f.Fuzz(func(t *testing.T, data []byte, depth uint8, n uint16) {
		d := uint(depth%32) + 1
		count := int(n%512) + 1
		out := make([]int32, count)
		// The contract is that it returns; the values on a malformed read are unspecified.
		_ = readSubframe(bitio.NewMSBReader(data), out, d)
	})
}

// newSubframeBuilderRaw makes a valid seed without a *testing.T.
func newSubframeBuilderRaw() []byte {
	w := bitio.NewMSBWriter()
	_ = w.Write(0, 1)
	_ = w.Write(8, 6) // fixed order 0
	_ = w.Write(0, 1)
	_ = w.Write(0, 2) // 4-bit rice
	_ = w.Write(0, 4) // partition order 0
	_ = w.Write(2, 4) // parameter
	for range 64 {
		_ = w.WriteUnary(0)
		_ = w.Write(0, 2)
	}
	return w.Bytes()
}
