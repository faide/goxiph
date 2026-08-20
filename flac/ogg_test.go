package flac

import (
	"bytes"
	"errors"
	"testing"
)

// mappingHeader builds a valid FLAC-in-Ogg first packet so the rejection cases can mutate it.
func mappingHeader(headerPackets int) []byte {
	p := make([]byte, 0, oggMappingHeaderLen)
	p = append(p, oggMappingMagic...)
	p = append(p, 1, 0) // mapping version 1.0
	p = append(p, byte(headerPackets>>8), byte(headerPackets))
	p = append(p, Signature...)

	// Stream info block header: last flag clear, type 0, length 34.
	p = append(p, 0x00, 0, 0, 34)

	info := make([]byte, 34)
	// Min and max block size, both 4096.
	info[0], info[1] = 0x10, 0x00
	info[2], info[3] = 0x10, 0x00

	// Sample rate, channel count and bit depth share a 28-bit group: 20 bits of rate, 3 of
	// (channels-1), then 5 of (depth-1) straddling the byte boundary.
	const (
		rate     = 44100
		channels = 2
		depth    = 16
	)
	packed := uint32(rate)<<4 | uint32(channels-1)<<1 | uint32(depth-1)>>4
	info[10] = byte(packed >> 16)
	info[11] = byte(packed >> 8)
	info[12] = byte(packed)
	info[13] = byte(depth-1) << 4 // low four bits of (depth-1); total samples stay zero

	return append(p, info...)
}

func TestParseOggMappingHeader(t *testing.T) {
	m, err := parseOggMappingHeader(mappingHeader(1))
	if err != nil {
		t.Fatalf("parseOggMappingHeader: %v", err)
	}
	if m.StreamInfo.SampleRate != 44100 {
		t.Errorf("SampleRate = %d, want 44100", m.StreamInfo.SampleRate)
	}
	if m.StreamInfo.Channels != 2 {
		t.Errorf("Channels = %d, want 2", m.StreamInfo.Channels)
	}
	if m.StreamInfo.BitsPerSample != 16 {
		t.Errorf("BitsPerSample = %d, want 16", m.StreamInfo.BitsPerSample)
	}
}

func TestParseOggMappingHeaderRejects(t *testing.T) {
	good := mappingHeader(1)

	cases := []struct {
		name string
		mut  func([]byte) []byte
	}{
		{"bad magic", func(p []byte) []byte { p[0] = 0x00; return p }},
		{"unsupported mapping version", func(p []byte) []byte { p[5] = 2; return p }},
		{"missing fLaC signature", func(p []byte) []byte { p[9] = 'X'; return p }},
		{"first block is not stream info", func(p []byte) []byte { p[13] = 4; return p }},
		{"truncated", func(p []byte) []byte { return p[:len(p)-1] }},
		{"empty", func([]byte) []byte { return nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bad := c.mut(append([]byte(nil), good...))
			if _, err := parseOggMappingHeader(bad); !errors.Is(err, ErrBadStream) {
				t.Errorf("got %v, want ErrBadStream", err)
			}
		})
	}
}

func TestOggHeaderPacketCount(t *testing.T) {
	for _, n := range []int{0, 1, 2, 300, 65535} {
		if got := oggHeaderPacketCount(mappingHeader(n)); got != n {
			t.Errorf("got %d, want %d", got, n)
		}
	}
	if got := oggHeaderPacketCount([]byte{0x7F}); got != 0 {
		t.Errorf("a truncated header gave %d, want 0", got)
	}
}

// TestNewDecoderRejectsUnknownContainer covers the sniffing step that chooses between the two
// framings.
func TestNewDecoderRejectsUnknownContainer(t *testing.T) {
	for _, data := range [][]byte{
		[]byte("RIFFxxxx"),
		[]byte("ID3\x04"),
		{0xFF, 0xF8, 0x00, 0x00},
		nil,
		[]byte("fLa"),
	} {
		if _, err := NewDecoder(bytes.NewReader(data)); err == nil {
			t.Errorf("accepted %q as a FLAC stream", data)
		}
	}
}
