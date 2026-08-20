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

// FuzzEncodeRoundTrip is the strongest property the codec has: whatever samples go in must come back
// out unchanged, for any signal the fuzzer invents.
func FuzzEncodeRoundTrip(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0}, uint8(16), uint8(1), uint16(64))
	f.Add([]byte{1, 2, 3, 4, 5, 6, 7, 8}, uint8(8), uint8(2), uint16(16))

	f.Fuzz(func(t *testing.T, raw []byte, depth, channels uint8, blockSize uint16) {
		d := int(depth%29) + 4         // 4..32
		ch := int(channels%8) + 1      // 1..8
		bs := int(blockSize%1024) + 16 // 16..1039
		if len(raw) < ch*2 {
			return
		}

		n := len(raw) / ch / 2
		if n == 0 {
			return
		}
		hi := int32(int64(1)<<(d-1)) - 1
		lo := -hi - 1

		samples := make([][]int32, ch)
		for c := range samples {
			samples[c] = make([]int32, n)
			for i := range n {
				off := (i*ch + c) * 2
				v := int32(int16(uint16(raw[off])<<8 | uint16(raw[off+1])))
				samples[c][i] = min(max(v, lo), hi)
			}
		}

		var buf seekableBuffer
		info := StreamInfo{SampleRate: 44100, Channels: ch, BitsPerSample: d}
		enc, err := NewEncoder(&buf, info, EncoderOptions{BlockSize: bs})
		if err != nil {
			t.Fatalf("NewEncoder: %v", err)
		}
		if err := enc.Write(samples); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := enc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		got, _, err := DecodeAll(bytes.NewReader(buf.data))
		if err != nil {
			t.Fatalf("decoding our own output: %v", err)
		}
		if len(got) != ch {
			t.Fatalf("got %d channels, want %d", len(got), ch)
		}
		for c := range samples {
			if len(got[c]) != n {
				t.Fatalf("channel %d: got %d samples, want %d", c, len(got[c]), n)
			}
			for i := range n {
				if got[c][i] != samples[c][i] {
					t.Fatalf("channel %d sample %d: got %d, want %d",
						c, i, got[c][i], samples[c][i])
				}
			}
		}
	})
}
