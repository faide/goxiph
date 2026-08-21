package opus

import (
	"encoding/binary"
	"errors"
	"testing"
)

// toc builds a TOC byte from its three fields.
func toc(config int, stereo bool, code int) byte {
	b := byte(config)<<3 | byte(code)
	if stereo {
		b |= 0x04
	}
	return b
}

// TestConfigTable checks the mapping of RFC 6716 table 2, where every boundary is a place a
// stream can be decoded with the wrong mode or frame size.
func TestConfigTable(t *testing.T) {
	cases := []struct {
		config    int
		mode      Mode
		bandwidth Bandwidth
		ms        float64
	}{
		{0, ModeSILK, BandwidthNarrow, 10},
		{3, ModeSILK, BandwidthNarrow, 60},
		{4, ModeSILK, BandwidthMedium, 10},
		{11, ModeSILK, BandwidthWide, 60},
		{12, ModeHybrid, BandwidthSuperWide, 10},
		{13, ModeHybrid, BandwidthSuperWide, 20},
		{14, ModeHybrid, BandwidthFull, 10},
		{15, ModeHybrid, BandwidthFull, 20},
		{16, ModeCELT, BandwidthNarrow, 2.5},
		{19, ModeCELT, BandwidthNarrow, 20},
		{20, ModeCELT, BandwidthWide, 2.5},
		{24, ModeCELT, BandwidthSuperWide, 2.5},
		{28, ModeCELT, BandwidthFull, 2.5},
		{31, ModeCELT, BandwidthFull, 20},
	}
	for _, c := range cases {
		got := configTable[c.config]
		if got.mode != c.mode {
			t.Errorf("config %d: mode %v, want %v", c.config, got.mode, c.mode)
		}
		if got.bandwidth != c.bandwidth {
			t.Errorf("config %d: bandwidth %v, want %v", c.config, got.bandwidth, c.bandwidth)
		}
		if wantNS := int64(c.ms * 1e6); got.durationNS != wantNS {
			t.Errorf("config %d: duration %d ns, want %d", c.config, got.durationNS, wantNS)
		}
	}

	// Every configuration must be filled in; a zero duration would mean a gap in the table.
	for i, c := range configTable {
		if c.durationNS == 0 {
			t.Errorf("config %d has no duration", i)
		}
	}
}

func TestCode0(t *testing.T) {
	data := append([]byte{toc(16, false, 0)}, 1, 2, 3, 4)
	p, err := ParsePacket(data)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if len(p.Frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(p.Frames))
	}
	if len(p.Frames[0]) != 4 {
		t.Errorf("frame is %d bytes, want 4", len(p.Frames[0]))
	}
	if p.Mode != ModeCELT || p.Stereo {
		t.Errorf("parsed as %v stereo=%v", p.Mode, p.Stereo)
	}
	// Config 16 is a 2.5 ms CELT frame: 120 samples at 48 kHz.
	if got := p.Samples(); got != 120 {
		t.Errorf("Samples = %d, want 120", got)
	}
}

func TestCode1(t *testing.T) {
	data := append([]byte{toc(0, true, 1)}, 1, 2, 3, 4, 5, 6)
	p, err := ParsePacket(data)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if len(p.Frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(p.Frames))
	}
	if len(p.Frames[0]) != 3 || len(p.Frames[1]) != 3 {
		t.Errorf("frames are %d and %d bytes, want 3 each", len(p.Frames[0]), len(p.Frames[1]))
	}
	if !p.Stereo || p.Channels() != 2 {
		t.Error("stereo flag did not survive")
	}
}

func TestCode1RejectsOddBody(t *testing.T) {
	data := append([]byte{toc(0, false, 1)}, 1, 2, 3)
	if _, err := ParsePacket(data); !errors.Is(err, ErrBadPacket) {
		t.Errorf("got %v, want ErrBadPacket", err)
	}
}

func TestCode2(t *testing.T) {
	// First frame is two bytes, the rest belongs to the second.
	data := append([]byte{toc(0, false, 2), 2}, 1, 2, 3, 4, 5)
	p, err := ParsePacket(data)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if len(p.Frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(p.Frames))
	}
	if len(p.Frames[0]) != 2 || len(p.Frames[1]) != 3 {
		t.Errorf("frames are %d and %d bytes, want 2 and 3", len(p.Frames[0]), len(p.Frames[1]))
	}
}

// TestTwoByteFrameLength covers the escape at 252, where a second byte joins the first.
func TestTwoByteFrameLength(t *testing.T) {
	// 252 + 4*1 = 256 bytes in the first frame.
	body := []byte{252, 1}
	body = append(body, make([]byte, 256+10)...)
	data := append([]byte{toc(0, false, 2)}, body...)

	p, err := ParsePacket(data)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if len(p.Frames[0]) != 256 {
		t.Errorf("first frame is %d bytes, want 256", len(p.Frames[0]))
	}
	if len(p.Frames[1]) != 10 {
		t.Errorf("second frame is %d bytes, want 10", len(p.Frames[1]))
	}
}

// TestZeroLengthFrameIsLegal covers the length that means the frame was dropped or never sent.
func TestZeroLengthFrameIsLegal(t *testing.T) {
	data := append([]byte{toc(0, false, 2), 0}, 1, 2, 3)
	p, err := ParsePacket(data)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if len(p.Frames[0]) != 0 {
		t.Errorf("first frame is %d bytes, want 0", len(p.Frames[0]))
	}
	if len(p.Frames[1]) != 3 {
		t.Errorf("second frame is %d bytes, want 3", len(p.Frames[1]))
	}
}

func TestCode3CBR(t *testing.T) {
	// Three frames of two bytes each, no padding, constant size.
	countByte := byte(3) // vbr clear, padding clear, M = 3
	data := append([]byte{toc(16, false, 3), countByte}, 1, 2, 3, 4, 5, 6)

	p, err := ParsePacket(data)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if len(p.Frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(p.Frames))
	}
	for i, f := range p.Frames {
		if len(f) != 2 {
			t.Errorf("frame %d is %d bytes, want 2", i, len(f))
		}
	}
	if got := p.Samples(); got != 360 { // three 2.5 ms frames
		t.Errorf("Samples = %d, want 360", got)
	}
}

func TestCode3VBR(t *testing.T) {
	// Three frames sized 1, 2 and whatever remains.
	countByte := byte(0x80 | 3) // vbr set, M = 3
	data := append([]byte{toc(16, false, 3), countByte, 1, 2}, 9, 8, 7, 6, 5)

	p, err := ParsePacket(data)
	if err != nil {
		t.Fatalf("ParsePacket: %v", err)
	}
	if len(p.Frames) != 3 {
		t.Fatalf("got %d frames, want 3", len(p.Frames))
	}
	if len(p.Frames[0]) != 1 || len(p.Frames[1]) != 2 || len(p.Frames[2]) != 2 {
		t.Errorf("frames are %d, %d and %d bytes, want 1, 2 and 2",
			len(p.Frames[0]), len(p.Frames[1]), len(p.Frames[2]))
	}
}

// TestCode3Padding covers the padding length encoding, including the 255 escape that lets a packet
// be padded to any size at all.
func TestCode3Padding(t *testing.T) {
	t.Run("single byte", func(t *testing.T) {
		countByte := byte(0x40 | 2) // padding set, M = 2
		body := []byte{countByte, 3}
		body = append(body, 1, 2, 3, 4) // two frames of two bytes
		body = append(body, 0, 0, 0)    // three padding bytes
		data := append([]byte{toc(16, false, 3)}, body...)

		p, err := ParsePacket(data)
		if err != nil {
			t.Fatalf("ParsePacket: %v", err)
		}
		if p.Padding != 3 {
			t.Errorf("Padding = %d, want 3", p.Padding)
		}
		if len(p.Frames) != 2 || len(p.Frames[0]) != 2 {
			t.Errorf("got %d frames, first of %d bytes", len(p.Frames), len(p.Frames[0]))
		}
	})

	t.Run("255 escape", func(t *testing.T) {
		// A value of 255 contributes 254 and continues into the next byte.
		countByte := byte(0x40 | 1)
		body := []byte{countByte, 255, 2}
		body = append(body, 7, 7)                 // the single frame
		body = append(body, make([]byte, 256)...) // 254 + 2 padding bytes
		data := append([]byte{toc(16, false, 3)}, body...)

		p, err := ParsePacket(data)
		if err != nil {
			t.Fatalf("ParsePacket: %v", err)
		}
		if p.Padding != 256 {
			t.Errorf("Padding = %d, want 256", p.Padding)
		}
		if len(p.Frames) != 1 || len(p.Frames[0]) != 2 {
			t.Errorf("got %d frames, first of %d bytes", len(p.Frames), len(p.Frames[0]))
		}
	})
}

func TestCode3Rejects(t *testing.T) {
	cases := []struct {
		name string
		data []byte
	}{
		{"no frame count byte", []byte{toc(16, false, 3)}},
		{"zero frames", []byte{toc(16, false, 3), 0}},
		{"cbr body not divisible", append([]byte{toc(16, false, 3), 3}, 1, 2, 3, 4)},
		{"padding exceeds body", append([]byte{toc(16, false, 3), 0x40 | 1, 200}, 1, 2)},
		{"truncated padding length", []byte{toc(16, false, 3), 0x40 | 1}},
		{"vbr length truncated", []byte{toc(16, false, 3), 0x80 | 3}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := ParsePacket(c.data); !errors.Is(err, ErrBadPacket) {
				t.Errorf("got %v, want ErrBadPacket", err)
			}
		})
	}
}

// TestPacketDurationLimit covers the 120 ms cap, which also bounds the frame count before any
// allocation follows from it.
func TestPacketDurationLimit(t *testing.T) {
	// Config 3 is a 60 ms SILK frame, so three of them exceed the limit.
	data := append([]byte{toc(3, false, 3), 3}, 1, 2, 3)
	if _, err := ParsePacket(data); !errors.Is(err, ErrBadPacket) {
		t.Errorf("got %v, want ErrBadPacket for 180 ms of audio", err)
	}

	// Two 60 ms frames are exactly at the limit and must be accepted.
	ok := append([]byte{toc(3, false, 3), 2}, 1, 2)
	p, err := ParsePacket(ok)
	if err != nil {
		t.Fatalf("rejected a packet at the limit: %v", err)
	}
	if p.DurationNS() != 120e6 {
		t.Errorf("DurationNS = %d, want 120000000", p.DurationNS())
	}
}

func TestFrameSizeLimit(t *testing.T) {
	// A single frame over 1275 bytes is not repacketizable and must be rejected.
	data := append([]byte{toc(16, false, 0)}, make([]byte, maxFrameSize+1)...)
	if _, err := ParsePacket(data); !errors.Is(err, ErrBadPacket) {
		t.Errorf("got %v, want ErrBadPacket", err)
	}
}

func TestEmptyPacketRejected(t *testing.T) {
	if _, err := ParsePacket(nil); !errors.Is(err, ErrBadPacket) {
		t.Errorf("got %v, want ErrBadPacket", err)
	}
}

// TestSamplesAcrossFrameSizes checks the 48 kHz sample count for every frame duration, including the
// 2.5 ms case where a millisecond-based calculation would lose the half.
func TestSamplesAcrossFrameSizes(t *testing.T) {
	cases := []struct {
		config int
		want   int
	}{
		{16, 120}, // 2.5 ms
		{17, 240}, // 5 ms
		{18, 480}, // 10 ms
		{19, 960}, // 20 ms
		{3, 2880}, // 60 ms
	}
	for _, c := range cases {
		data := append([]byte{toc(c.config, false, 0)}, 1, 2, 3)
		p, err := ParsePacket(data)
		if err != nil {
			t.Fatalf("config %d: %v", c.config, err)
		}
		if got := p.Samples(); got != c.want {
			t.Errorf("config %d: Samples = %d, want %d", c.config, got, c.want)
		}
	}
}

func FuzzParsePacket(f *testing.F) {
	f.Add([]byte{toc(16, false, 0), 1, 2, 3})
	f.Add([]byte{toc(0, true, 3), 0x80 | 2, 1})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := ParsePacket(data)
		if err != nil {
			return
		}
		// Anything that parsed must be self-consistent, since a decoder will trust it.
		if len(p.Frames) == 0 {
			t.Fatal("a parsed packet has no frames")
		}
		if p.DurationNS() > maxPacketDurationMS*1e6 {
			t.Fatalf("packet carries %d ns, over the limit", p.DurationNS())
		}
		total := p.Padding
		for _, fr := range p.Frames {
			if len(fr) > maxFrameSize {
				t.Fatalf("frame of %d bytes exceeds the limit", len(fr))
			}
			total += len(fr)
		}
		if total > len(data) {
			t.Fatalf("frames and padding total %d bytes from a %d byte packet", total, len(data))
		}
	})
}

// buildHead assembles an identification header for a mapping family test.
func buildHead(channels, family int, tail []byte) []byte {
	h := make([]byte, 19)
	copy(h, "OpusHead")
	h[8] = 1
	h[9] = byte(channels)
	binary.LittleEndian.PutUint16(h[10:], 312)
	binary.LittleEndian.PutUint32(h[12:], 48000)
	h[18] = byte(family)
	return append(h, tail...)
}

// TestHeadAmbisonicsFamilyTwo checks that family 2 reads the same mapping table as family 1.
//
// RFC 8486 section 3.1 gives ambisonics the layout family 1 already uses, so nothing about the
// parsing changes; what changes is that the family is no longer reserved and must be accepted.
func TestHeadAmbisonicsFamilyTwo(t *testing.T) {
	h, err := ParseHead(buildHead(4, MappingFamilyAmbisonics, []byte{3, 1, 0, 1, 2, 3}))
	if err != nil {
		t.Fatalf("family 2: %v", err)
	}
	if h.Undecodable {
		t.Error("family 2 was treated as unknown")
	}
	if h.StreamCount != 3 || h.CoupledCount != 1 {
		t.Errorf("streams %d coupled %d, want 3 and 1", h.StreamCount, h.CoupledCount)
	}
	if len(h.ChannelMapping) != 4 {
		t.Errorf("mapping holds %d channels, want 4", len(h.ChannelMapping))
	}
	if len(h.DemixingMatrix) != 0 {
		t.Error("family 2 produced a demixing matrix, which only family 3 carries")
	}
}

// TestHeadAmbisonicsMatrixFamilyThree covers the one family whose mapping table has a different
// shape: a matrix in place of the per-channel mapping. RFC 8486 section 3.2.
func TestHeadAmbisonicsMatrixFamilyThree(t *testing.T) {
	// Two streams, one coupled, so three decoded channels feeding two output channels.
	const streams, coupled, channels = 2, 1, 2
	decoded := streams + coupled

	tail := []byte{streams, coupled}
	for i := range decoded * channels {
		tail = binary.LittleEndian.AppendUint16(tail, uint16(int16(-1000+i*500)))
	}

	h, err := ParseHead(buildHead(channels, MappingFamilyAmbisonicsMatrix, tail))
	if err != nil {
		t.Fatalf("family 3: %v", err)
	}
	if len(h.DemixingMatrix) != decoded*channels {
		t.Fatalf("matrix holds %d entries, want %d", len(h.DemixingMatrix), decoded*channels)
	}
	for i, v := range h.DemixingMatrix {
		if want := int16(-1000 + i*500); v != want {
			t.Errorf("matrix entry %d is %d, want %d", i, v, want)
		}
	}
	if len(h.ChannelMapping) != 0 {
		t.Error("family 3 produced a channel mapping, which the matrix replaces")
	}

	// A matrix that runs past the header is a truncated header, not a short matrix.
	if _, err := ParseHead(buildHead(channels, MappingFamilyAmbisonicsMatrix, tail[:len(tail)-2])); err == nil {
		t.Error("a truncated demixing matrix was accepted")
	}
}

// TestHeadUnknownFamilyStopsAtNineteenOctets covers the rule RFC 8486 section 5.2 replaced.
//
// The earlier text had a demuxer treat an unrecognised family as family 255, which means reading a
// mapping table whose layout that family need not use. Nothing past the fixed nineteen octets can be
// relied on, so nothing past them is read.
func TestHeadUnknownFamilyStopsAtNineteenOctets(t *testing.T) {
	// A tail that would be a valid family 1 table, to show it is not being read.
	h, err := ParseHead(buildHead(2, 7, []byte{9, 9, 9, 9}))
	if err != nil {
		t.Fatalf("unknown family: %v", err)
	}
	if !h.Undecodable {
		t.Fatal("an unknown family was not marked undecodable")
	}
	if h.Channels != 2 || h.PreSkip != 312 || h.InputRate != 48000 {
		t.Error("the fixed part of the header was not read")
	}
	if h.StreamCount != 0 || len(h.ChannelMapping) != 0 || len(h.DemixingMatrix) != 0 {
		t.Error("a mapping table was read for a family whose layout is unknown")
	}

	// The header must still parse with nothing after the family octet at all.
	if _, err := ParseHead(buildHead(2, 7, nil)); err != nil {
		t.Errorf("a bare unknown-family header was rejected: %v", err)
	}
}
