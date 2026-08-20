// Package opus implements the Opus audio codec of RFC 6716 and its Ogg mapping of RFC 7845.
package opus

import (
	"errors"
	"fmt"
)

// ErrBadPacket reports a packet that violates RFC 6716 section 3.
var ErrBadPacket = errors.New("opus: malformed packet")

// Mode is the operating mode a configuration selects.
type Mode int

// The three operating modes. SILK is a linear-prediction coder for speech, CELT is an MDCT coder,
// and Hybrid runs SILK below and CELT above a crossover.
const (
	ModeSILK Mode = iota
	ModeHybrid
	ModeCELT
)

func (m Mode) String() string {
	switch m {
	case ModeSILK:
		return "SILK"
	case ModeHybrid:
		return "Hybrid"
	case ModeCELT:
		return "CELT"
	default:
		return "unknown"
	}
}

// Bandwidth is the audio bandwidth a configuration selects.
type Bandwidth int

// The five bandwidths, from narrowband to fullband.
const (
	BandwidthNarrow Bandwidth = iota
	BandwidthMedium
	BandwidthWide
	BandwidthSuperWide
	BandwidthFull
)

func (b Bandwidth) String() string {
	switch b {
	case BandwidthNarrow:
		return "NB"
	case BandwidthMedium:
		return "MB"
	case BandwidthWide:
		return "WB"
	case BandwidthSuperWide:
		return "SWB"
	case BandwidthFull:
		return "FB"
	default:
		return "unknown"
	}
}

// SampleRate is the rate every Opus stream is decoded and timed at, whatever its internal bandwidth.
const SampleRate = 48000

// maxFrameSize is the largest compressed frame the length coding can express.
const maxFrameSize = 1275

// maxPacketDurationMS bounds the audio one packet may carry. RFC 6716 section 3.2.5.
const maxPacketDurationMS = 120

// config describes one of the 32 TOC configurations. RFC 6716 table 2.
type config struct {
	mode      Mode
	bandwidth Bandwidth
	// durationNS is the frame length in nanoseconds, which keeps 2.5 ms exact in integers.
	durationNS int64
}

// configTable maps the five-bit configuration number onto its parameters.
var configTable = buildConfigTable()

func buildConfigTable() [32]config {
	var t [32]config

	silkSizes := [4]int64{10e6, 20e6, 40e6, 60e6}
	for i, bw := range [3]Bandwidth{BandwidthNarrow, BandwidthMedium, BandwidthWide} {
		for j, d := range silkSizes {
			t[i*4+j] = config{ModeSILK, bw, d}
		}
	}

	hybridSizes := [2]int64{10e6, 20e6}
	for i, bw := range [2]Bandwidth{BandwidthSuperWide, BandwidthFull} {
		for j, d := range hybridSizes {
			t[12+i*2+j] = config{ModeHybrid, bw, d}
		}
	}

	celtSizes := [4]int64{2.5e6, 5e6, 10e6, 20e6}
	for i, bw := range [4]Bandwidth{BandwidthNarrow, BandwidthWide, BandwidthSuperWide, BandwidthFull} {
		for j, d := range celtSizes {
			t[16+i*4+j] = config{ModeCELT, bw, d}
		}
	}
	return t
}

// Packet is a parsed Opus packet: its configuration and the frames it carries.
type Packet struct {
	Config    int
	Mode      Mode
	Bandwidth Bandwidth
	Stereo    bool

	// FrameDurationNS is the length of each frame, in nanoseconds so 2.5 ms stays exact.
	FrameDurationNS int64

	// Frames holds the compressed frames, each aliasing the packet. A frame may be empty, which
	// signals that it was dropped in transit or that the encoder chose not to send it.
	Frames [][]byte

	// Padding is the Opus-layer padding the packet carried, which decoders ignore.
	Padding int
}

// Samples reports the audio the packet carries, at the 48 kHz rate Opus always decodes to.
func (p *Packet) Samples() int {
	return int(int64(len(p.Frames)) * p.FrameDurationNS * SampleRate / 1e9)
}

// DurationNS reports the audio the packet carries, in nanoseconds.
func (p *Packet) DurationNS() int64 {
	return int64(len(p.Frames)) * p.FrameDurationNS
}

// Channels reports the channel count the TOC byte signals.
func (p *Packet) Channels() int {
	if p.Stereo {
		return 2
	}
	return 1
}

// ParsePacket splits a packet into its frames. RFC 6716 section 3.
//
// The returned frames alias data; the caller must copy them to outlive it.
func ParsePacket(data []byte) (*Packet, error) {
	if len(data) < 1 {
		return nil, fmt.Errorf("%w: empty", ErrBadPacket)
	}

	toc := data[0]
	cfg := configTable[toc>>3]
	p := &Packet{
		Config:          int(toc >> 3),
		Mode:            cfg.mode,
		Bandwidth:       cfg.bandwidth,
		Stereo:          toc&0x04 != 0,
		FrameDurationNS: cfg.durationNS,
	}

	body := data[1:]
	switch toc & 0x03 {
	case 0:
		if err := p.parseCode0(body); err != nil {
			return nil, err
		}
	case 1:
		if err := p.parseCode1(body); err != nil {
			return nil, err
		}
	case 2:
		if err := p.parseCode2(body); err != nil {
			return nil, err
		}
	default:
		if err := p.parseCode3(body); err != nil {
			return nil, err
		}
	}

	if d := p.DurationNS(); d > maxPacketDurationMS*1e6 {
		return nil, fmt.Errorf("%w: %d ms of audio exceeds the %d ms limit",
			ErrBadPacket, d/1e6, maxPacketDurationMS)
	}
	for i, f := range p.Frames {
		if len(f) > maxFrameSize {
			return nil, fmt.Errorf("%w: frame %d is %d bytes, over the %d byte limit",
				ErrBadPacket, i, len(f), maxFrameSize)
		}
	}
	return p, nil
}

// parseCode0 reads a packet holding one frame, which occupies the whole body.
func (p *Packet) parseCode0(body []byte) error {
	p.Frames = [][]byte{body}
	return nil
}

// parseCode1 reads two frames of equal size.
func (p *Packet) parseCode1(body []byte) error {
	if len(body)%2 != 0 {
		return fmt.Errorf("%w: code 1 body of %d bytes is not divisible in two", ErrBadPacket, len(body))
	}
	half := len(body) / 2
	p.Frames = [][]byte{body[:half], body[half:]}
	return nil
}

// parseCode2 reads two frames, the first with an explicit length.
func (p *Packet) parseCode2(body []byte) error {
	n, rest, err := readFrameLength(body)
	if err != nil {
		return err
	}
	if n > len(rest) {
		return fmt.Errorf("%w: code 2 first frame wants %d bytes, %d remain", ErrBadPacket, n, len(rest))
	}
	p.Frames = [][]byte{rest[:n], rest[n:]}
	return nil
}

// parseCode3 reads a signalled number of frames, with optional padding and either constant or
// variable frame sizes. RFC 6716 section 3.2.5.
func (p *Packet) parseCode3(body []byte) error {
	if len(body) < 1 {
		return fmt.Errorf("%w: code 3 packet has no frame count byte", ErrBadPacket)
	}

	countByte := body[0]
	body = body[1:]
	vbr := countByte&0x80 != 0
	hasPadding := countByte&0x40 != 0
	count := int(countByte & 0x3F)
	if count == 0 {
		return fmt.Errorf("%w: code 3 frame count is zero", ErrBadPacket)
	}
	// The duration limit bounds the count before any allocation follows from it.
	if int64(count)*p.FrameDurationNS > maxPacketDurationMS*1e6 {
		return fmt.Errorf("%w: %d frames exceed the %d ms limit", ErrBadPacket, count, maxPacketDurationMS)
	}

	if hasPadding {
		// A padding length of 255 means 254 bytes plus whatever the next byte adds, so the count is
		// spread over as many bytes as it takes.
		total := 0
		for {
			if len(body) < 1 {
				return fmt.Errorf("%w: truncated padding length", ErrBadPacket)
			}
			v := int(body[0])
			body = body[1:]
			if v < 255 {
				total += v
				break
			}
			total += 254
		}
		if total > len(body) {
			return fmt.Errorf("%w: %d padding bytes exceed the %d remaining", ErrBadPacket, total, len(body))
		}
		p.Padding = total
		body = body[:len(body)-total]
	}

	if !vbr {
		if len(body)%count != 0 {
			return fmt.Errorf("%w: %d bytes do not divide into %d equal frames", ErrBadPacket, len(body), count)
		}
		size := len(body) / count
		p.Frames = make([][]byte, count)
		for i := range count {
			p.Frames[i] = body[i*size : (i+1)*size]
		}
		return nil
	}

	// Variable size: every frame but the last carries its own length.
	sizes := make([]int, count-1)
	for i := range count - 1 {
		n, rest, err := readFrameLength(body)
		if err != nil {
			return err
		}
		sizes[i] = n
		body = rest
	}

	p.Frames = make([][]byte, count)
	for i, n := range sizes {
		if n > len(body) {
			return fmt.Errorf("%w: frame %d wants %d bytes, %d remain", ErrBadPacket, i, n, len(body))
		}
		p.Frames[i] = body[:n]
		body = body[n:]
	}
	p.Frames[count-1] = body
	return nil
}

// readFrameLength decodes a one- or two-byte frame length. RFC 6716 section 3.2.1.
//
// A length of zero is legal and means the frame is absent, either dropped in transit or never sent.
func readFrameLength(body []byte) (int, []byte, error) {
	if len(body) < 1 {
		return 0, body, fmt.Errorf("%w: truncated frame length", ErrBadPacket)
	}
	first := int(body[0])
	if first < 252 {
		return first, body[1:], nil
	}
	if len(body) < 2 {
		return 0, body, fmt.Errorf("%w: truncated two-byte frame length", ErrBadPacket)
	}
	return int(body[1])*4 + first, body[2:], nil
}
