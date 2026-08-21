package opus

import (
	"fmt"

	"github.com/faide/goxiph/internal/celt"
	"github.com/faide/goxiph/internal/rangecoder"
	"github.com/faide/goxiph/internal/silk"
)

// An Opus packet is decoded by one of two codecs, or by both. Which one is fixed by the table of
// contents byte, and it may change from packet to packet: a stream moves between them as the signal
// and the rate change, and a decoder has to follow without a gap.
//
// RFC 6716 section 4.

// OutputRate is the rate Opus always decodes to, whatever the codecs run at internally.
const OutputRate = 48000

// endBandFor maps a bandwidth onto the first CELT band a frame does not code, from
// src/opus_decoder.c of the reference implementation.
func endBandFor(b Bandwidth) int {
	switch b {
	case BandwidthNarrow:
		return 13
	case BandwidthMedium, BandwidthWide:
		return 17
	case BandwidthSuperWide:
		return 19
	default:
		return 21
	}
}

// silkRateFor maps a bandwidth onto SILK's internal rate in kilohertz, or zero where SILK does not
// code that width.
func silkRateFor(b Bandwidth) int {
	switch b {
	case BandwidthNarrow:
		return silk.NarrowBand
	case BandwidthMedium:
		return silk.MediumBand
	case BandwidthWide:
		return silk.WideBand
	default:
		return 0
	}
}

// Decoder turns Opus packets into samples.
//
// It holds one decoder per codec, because both carry state across packets and neither can be rebuilt
// mid-stream without a discontinuity. A configuration change that either cannot absorb rebuilds only
// that one.
type Decoder struct {
	channels int

	celt     *celt.Decoder
	celtEnd  int
	celtChan int

	silk     *silk.Decoder
	silkRate int
	silkChan int
}

// NewDecoder returns a decoder producing the given channel count at 48 kHz.
func NewDecoder(channels int) (*Decoder, error) {
	if channels != 1 && channels != 2 {
		return nil, fmt.Errorf("opus: %d channels", channels)
	}
	return &Decoder{channels: channels}, nil
}

// Decode returns one packet's samples, one slice per channel, in the range minus one to one.
//
// The channel count is the stream's, not the packet's: a packet that codes fewer channels than the
// stream declares has its output duplicated, which is how a stream drops to mono without changing
// what it delivers.
func (d *Decoder) Decode(packet []byte) ([][]float32, error) {
	p, err := ParsePacket(packet)
	if err != nil {
		return nil, err
	}

	coded := 1
	if p.Stereo {
		coded = 2
	}
	frameSamples := p.Samples() / len(p.Frames)

	out := make([][]float32, coded)
	for _, frame := range p.Frames {
		got, err := d.decodeFrame(p, frame, frameSamples, coded)
		if err != nil {
			return nil, err
		}
		for c := range out {
			out[c] = append(out[c], got[c]...)
		}
	}

	// Widen or narrow to the stream's channel count.
	for len(out) < d.channels {
		out = append(out, append([]float32(nil), out[0]...))
	}
	return out[:d.channels], nil
}

// decodeFrame decodes one frame of a packet with whichever codec its configuration names.
func (d *Decoder) decodeFrame(p *Packet, frame []byte, samples, coded int) ([][]float32, error) {
	if len(frame) <= 1 {
		// An empty frame is silence, and neither codec is run for it.
		out := make([][]float32, coded)
		for c := range out {
			out[c] = make([]float32, samples)
		}
		return out, nil
	}

	switch p.Mode {
	case ModeCELT:
		return d.decodeCELT(p, frame, samples, coded)
	case ModeSILK:
		return d.decodeSILK(p, frame, samples, coded)
	default:
		return nil, fmt.Errorf("opus: %s frames are not supported yet", p.Mode)
	}
}

func (d *Decoder) decodeCELT(p *Packet, frame []byte, samples, coded int) ([][]float32, error) {
	end := endBandFor(p.Bandwidth)
	if d.celt == nil || d.celtEnd != end || d.celtChan != coded {
		dec, err := celt.NewDecoder(coded, 0, end)
		if err != nil {
			return nil, err
		}
		d.celt, d.celtEnd, d.celtChan = dec, end, coded
	}

	size, err := celt.FrameSizeForSamples(samples)
	if err != nil {
		return nil, err
	}
	decoded, err := d.celt.DecodeFrame(rangecoder.NewDecoder(frame), len(frame), size)
	if err != nil {
		return nil, err
	}
	return decoded.PCM, nil
}

func (d *Decoder) decodeSILK(p *Packet, frame []byte, samples, coded int) ([][]float32, error) {
	rate := silkRateFor(p.Bandwidth)
	if rate == 0 {
		return nil, fmt.Errorf("opus: SILK does not code %s", p.Bandwidth)
	}
	frameMS := samples / (OutputRate / 1000)
	// A change of packet duration is not a reason to start again: the filters and the resampler
	// carry across it. Only the internal rate or the channel count forces a new decoder, because
	// neither can be reinterpreted.
	if d.silk == nil || d.silkRate != rate || d.silkChan != coded {
		dec, err := silk.NewDecoder(silk.Config{
			SampleRateKHz: rate,
			FrameMS:       frameMS,
			Channels:      coded,
		})
		if err != nil {
			return nil, err
		}
		d.silk, d.silkRate, d.silkChan = dec, rate, coded
	}

	pcm, err := d.silk.Decode(rangecoder.NewDecoder(frame), frameMS)
	if err != nil {
		return nil, err
	}

	out := make([][]float32, coded)
	for c := range out {
		// A mono payload in a stereo packet is not possible: the coded count comes from the same
		// byte SILK was told about, so every coded channel is present.
		out[c] = make([]float32, len(pcm[c]))
		for i, v := range pcm[c] {
			out[c][i] = float32(v) / 32768
		}
	}
	return out, nil
}
