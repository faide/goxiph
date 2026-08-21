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

// hybridStartBand is the first CELT band of a hybrid packet. Everything below it belongs to SILK,
// which is why the two can share a packet without either hearing the other's bands.
const hybridStartBand = 17

// hybridSILKRate is the rate SILK always runs at in a hybrid packet, whatever the packet's
// bandwidth: the wider part is CELT's to carry.
const hybridSILKRate = silk.WideBand

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

	celt      *celt.Decoder
	celtStart int
	celtEnd   int
	celtChan  int

	silk     *silk.Decoder
	silkRate int
	silkChan int

	// prevMode is what the last frame used. A change of mode invalidates the state of whichever
	// codec was not running, because it has missed however long the other was in charge.
	prevMode Mode
	hasPrev  bool
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

	// A codec that was not running has missed the intervening signal, and cannot predict across the
	// gap. Whichever one is resumed here starts again.
	if d.hasPrev && d.prevMode != p.Mode {
		if p.Mode != ModeCELT && d.prevMode == ModeCELT {
			d.silk = nil
		}
		if p.Mode != ModeSILK && d.prevMode == ModeSILK {
			d.celt = nil
		}
	}
	d.prevMode, d.hasPrev = p.Mode, true

	switch p.Mode {
	case ModeCELT:
		return d.decodeCELT(p, rangecoder.NewDecoder(frame), len(frame), samples, coded, 0)
	case ModeSILK:
		dec := rangecoder.NewDecoder(frame)
		out, err := d.decodeSILK(p, dec, samples, coded, silkRateFor(p.Bandwidth))
		if err != nil {
			return nil, err
		}
		// A SILK-only packet can carry a redundant transform frame too, and its bytes are not
		// SILK's. Nothing reads them here, but the flags have to be accounted for.
		readRedundancy(dec, len(frame), false)
		return out, nil
	case ModeHybrid:
		// One range decoder, read in turn: SILK takes the low bands and leaves the coder wherever
		// its symbols end, and CELT carries on from there. Reading them in the other order, or from
		// two decoders, gives each the other's bits.
		dec := rangecoder.NewDecoder(frame)
		low, err := d.decodeSILK(p, dec, samples, coded, hybridSILKRate)
		if err != nil {
			return nil, err
		}
		length := readRedundancy(dec, len(frame), true)
		high, err := d.decodeCELT(p, dec, length, samples, coded, hybridStartBand)
		if err != nil {
			return nil, err
		}
		// The two cover different bands of the same signal, so the result is their sum.
		for c := range high {
			for i := range high[c] {
				if i < len(low[c]) {
					high[c][i] += low[c][i]
				}
			}
		}
		return high, nil
	default:
		return nil, fmt.Errorf("opus: %s frames are not supported yet", p.Mode)
	}
}

func (d *Decoder) decodeCELT(p *Packet, dec *rangecoder.Decoder, length, samples, coded, start int) ([][]float32, error) {
	end := endBandFor(p.Bandwidth)
	if d.celt == nil || d.celtStart != start || d.celtEnd != end || d.celtChan != coded {
		c, err := celt.NewDecoder(coded, start, end)
		if err != nil {
			return nil, err
		}
		d.celt, d.celtStart, d.celtEnd, d.celtChan = c, start, end, coded
	}

	size, err := celt.FrameSizeForSamples(samples)
	if err != nil {
		return nil, err
	}
	decoded, err := d.celt.DecodeFrame(dec, length, size)
	if err != nil {
		return nil, err
	}
	return decoded.PCM, nil
}

func (d *Decoder) decodeSILK(p *Packet, dec *rangecoder.Decoder, samples, coded, rate int) ([][]float32, error) {
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

	pcm, err := d.silk.Decode(dec, frameMS)
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

// readRedundancy reads the flags that may sit between SILK's symbols and CELT's, and returns how
// many bytes of the frame remain for CELT.
//
// A packet that changes between the two codecs can carry a short redundant frame of the other one,
// so that the transition is crossfaded rather than cut. The flags are only present where there is
// room for them, which is why the position is tested before anything is read.
//
// The redundant audio itself is not decoded here. Nothing in the corpus signals it, and using it
// would only smooth a transition; the accounting is what the rest of the packet depends on.
func readRedundancy(dec *rangecoder.Decoder, length int, hybrid bool) int {
	need := 17
	if hybrid {
		need += 20
	}
	if dec.Tell()+need > 8*length {
		return length
	}

	redundant := true
	if hybrid {
		redundant = dec.DecodeBitLogp(12) != 0
	}
	if !redundant {
		return length
	}

	dec.DecodeBitLogp(1) // whether the redundancy carries the transform codec into SILK or out of it
	var bytes int
	if hybrid {
		bytes = int(dec.DecodeUint(256)) + 2
	} else {
		bytes = length - (dec.Tell()+7)>>3
	}

	if length-bytes < 0 || (length-bytes)*8 < dec.Tell() {
		// Not a shape a valid packet takes; leaving the frame whole is the safe reading.
		return length
	}
	// The raw bits CELT reads backwards must stop before the redundant frame's bytes.
	dec.Shrink(bytes)
	return length - bytes
}
