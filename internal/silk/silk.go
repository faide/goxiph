// Package silk implements the linear-prediction layer of the Opus codec, RFC 6716 section 4.2.
//
// SILK models speech as an excitation driving a short-term filter that stands for the vocal tract
// and, when the signal is voiced, a long-term filter that stands for the pitch. Coding those filters
// separately from the excitation is what lets it carry intelligible speech at rates where a
// transform codec has nothing left to spend.
package silk

import (
	"fmt"

	"github.com/faide/goxiph/internal/rangecoder"
)

// The bandwidths SILK codes at, as internal sample rates in kilohertz. Anything wider is CELT's.
const (
	NarrowBand = 8
	MediumBand = 12
	WideBand   = 16
)

// MaxFramesPerPacket is the most 20 ms frames a SILK packet can carry, at 60 ms.
const MaxFramesPerPacket = 3

// SubframesPerFrame is the number of subframes a frame divides into. A 10 ms frame has half as many.
const (
	SubframesPerFrame   = 4
	Subframes10ms       = 2
	SubframeLengthMS    = 5
	MaxSubframesInFrame = SubframesPerFrame
)

// Config describes how a packet's SILK payload is laid out. It comes from the Opus table of
// contents rather than from the payload itself.
type Config struct {
	// SampleRateKHz is the internal rate: 8, 12 or 16.
	SampleRateKHz int
	// FrameMS is the packet's duration, one of 10, 20, 40 or 60.
	FrameMS int
	// Channels is 1 or 2.
	Channels int
}

// FramesPerPacket returns how many 20 ms frames the packet carries. A 10 ms packet carries one.
func (c Config) FramesPerPacket() int {
	if c.FrameMS <= 20 {
		return 1
	}
	return c.FrameMS / 20
}

// Subframes returns the subframes per frame, which is halved for a 10 ms packet.
func (c Config) Subframes() int {
	if c.FrameMS == 10 {
		return Subframes10ms
	}
	return SubframesPerFrame
}

// FrameLength returns a frame's length in samples at the internal rate.
func (c Config) FrameLength() int {
	return c.Subframes() * SubframeLengthMS * c.SampleRateKHz
}

// Validate reports whether the configuration is one SILK defines.
func (c Config) Validate() error {
	switch c.SampleRateKHz {
	case NarrowBand, MediumBand, WideBand:
	default:
		return fmt.Errorf("silk: %d kHz is not a SILK rate", c.SampleRateKHz)
	}
	switch c.FrameMS {
	case 10, 20, 40, 60:
	default:
		return fmt.Errorf("silk: %d ms is not a SILK packet length", c.FrameMS)
	}
	if c.Channels != 1 && c.Channels != 2 {
		return fmt.Errorf("silk: %d channels", c.Channels)
	}
	return nil
}

// Header is what a packet declares before any frame is coded.
type Header struct {
	// VAD holds, per channel and frame, whether the frame carries active speech. An inactive frame
	// is still coded, but from a different distribution and usually with far fewer bits.
	VAD [2][MaxFramesPerPacket]bool
	// LBRR reports that a channel carries redundant copies of earlier frames, and which ones.
	LBRR      [2]bool
	LBRRFrame [2][MaxFramesPerPacket]bool
}

// DecodeHeader reads the per-packet flags that precede every SILK frame.
//
// The order matters and is not the obvious one: every channel's activity flags come first, each
// followed by that channel's redundancy flag, and only then the redundancy detail for both. Reading
// them channel-major would consume the same number of bits in a different order and decode a
// different packet.
func DecodeHeader(d *rangecoder.Decoder, c Config) (Header, error) {
	if err := c.Validate(); err != nil {
		return Header{}, err
	}

	var h Header
	frames := c.FramesPerPacket()

	for ch := range c.Channels {
		for i := range frames {
			h.VAD[ch][i] = d.DecodeBitLogp(1) != 0
		}
		h.LBRR[ch] = d.DecodeBitLogp(1) != 0
	}

	for ch := range c.Channels {
		if !h.LBRR[ch] {
			continue
		}
		if frames == 1 {
			h.LBRRFrame[ch][0] = true
			continue
		}
		// With more than one frame the set is coded as a symbol, whose value is a bitmap offset by
		// one: the empty set is not codeable, because the flag above already ruled it out.
		symbol := d.DecodeICDF(lbrrFlagsICDF(frames), 8) + 1
		for i := range frames {
			h.LBRRFrame[ch][i] = symbol>>uint(i)&1 != 0
		}
	}
	return h, nil
}

// lbrrFlagsICDF returns the distribution for a packet of the given frame count.
func lbrrFlagsICDF(frames int) []byte {
	if frames == 2 {
		return lbrrFlags2ICDF[:]
	}
	return lbrrFlags3ICDF[:]
}
