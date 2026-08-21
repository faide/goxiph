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

	celt *celt.Decoder

	silk     *silk.Decoder
	silkRate int
	silkChan int

	// prevMode is what the last frame used. A change of mode invalidates the state of whichever
	// codec was not running, because it has missed however long the other was in charge.
	prevMode Mode
	hasPrev  bool
	// prevRedundancy records that the previous packet carried a redundant frame going into the
	// transform codec, which spares the next one a reset.
	prevRedundancy bool

	// finalRange is the entropy coder's state after the last frame decoded. It is a running
	// function of every symbol read, so two decoders agree on it only if they read the same
	// symbols in the same order. RFC 6716 section 5.1 names it as the way to find a fault in
	// either side.
	finalRange uint32
}

// FinalRange returns the entropy coder state after the most recent frame.
func (d *Decoder) FinalRange() uint32 { return d.finalRange }

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

	out := make([][]float32, d.channels)
	for _, frame := range p.Frames {
		got, err := d.decodeFrame(p, frame, frameSamples, coded)
		if err != nil {
			return nil, err
		}
		for c := range out {
			out[c] = append(out[c], got[c]...)
		}
	}

	return out, nil
}

// decodeFrame decodes one frame of a packet with whichever codec its configuration names.
func (d *Decoder) decodeFrame(p *Packet, frame []byte, samples, coded int) ([][]float32, error) {
	if len(frame) <= 1 {
		// An empty frame is silence, and neither codec is run for it. The reference reports no
		// range for such a packet, so neither does this.
		d.finalRange = 0
		out := make([][]float32, d.channels)
		for c := range out {
			out[c] = make([]float32, samples)
		}
		return out, nil
	}

	// A change of mode starts the transform codec again, because it has either missed the
	// intervening signal or is about to hand it over; a packet whose predecessor carried redundancy
	// is the exception, since the redundant frame already bridged the gap. The linear predictor
	// starts again only when coming back from the transform codec alone.
	if d.hasPrev && d.prevMode != p.Mode {
		if !d.prevRedundancy {
			d.celt = nil
		}
		if d.prevMode == ModeCELT {
			d.silk = nil
		}
	}
	d.prevMode, d.hasPrev = p.Mode, true
	d.prevRedundancy = false

	switch p.Mode {
	case ModeCELT:
		dec := rangecoder.NewDecoder(frame)
		out, err := d.decodeCELT(p, dec, len(frame), samples, coded, 0)
		d.finalRange = dec.Range()
		return out, err
	case ModeSILK:
		dec := rangecoder.NewDecoder(frame)
		out, err := d.decodeSILK(p, dec, samples, coded, silkRateFor(p.Bandwidth))
		if err != nil {
			return nil, err
		}
		red := readRedundancy(dec, len(frame), false)
		if err := d.applyRedundancy(p, frame, red, out, coded, dec); err != nil {
			return nil, err
		}
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
		red := readRedundancy(dec, len(frame), true)

		// Going into SILK the redundant frame is read first, because the main frame's decode
		// continues from the state it leaves.
		var early [][]float32
		var redRange uint32
		if red.present && red.celtToSilk {
			early, redRange, err = d.decodeRedundant(p, frame, red, coded)
			if err != nil {
				return nil, err
			}
		}

		high, err := d.decodeCELT(p, dec, red.length, samples, coded, hybridStartBand)
		if err != nil {
			return nil, err
		}
		mainRange := dec.Range()

		// The two cover different bands of the same signal, so the result is their sum.
		for c := range high {
			for i := range high[c] {
				if i < len(low[c]) {
					high[c][i] += low[c][i]
				}
			}
		}

		if red.present && !red.celtToSilk {
			early, redRange, err = d.decodeRedundant(p, frame, red, coded)
			if err != nil {
				return nil, err
			}
		}
		d.blendRedundancy(high, early, red, samples)
		d.prevRedundancy = red.present && !red.celtToSilk
		d.finalRange = mainRange ^ redRange
		return high, nil
	default:
		return nil, fmt.Errorf("opus: %s frames are not supported yet", p.Mode)
	}
}

func (d *Decoder) decodeCELT(p *Packet, dec *rangecoder.Decoder, length, samples, coded, start int) ([][]float32, error) {
	end := endBandFor(p.Bandwidth)
	if d.celt == nil {
		// Built for what the stream delivers, not for what this packet codes.
		c, err := celt.NewDecoder(d.channels, start, end)
		if err != nil {
			return nil, err
		}
		d.celt = c
	}
	if err := d.celt.Configure(coded, start, end); err != nil {
		return nil, err
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
	// carry across it. Nor is a change of rate, quite: that clears the filters but keeps what the
	// entropy decoding refers back to, which is the decoder's own business. Only a change of
	// channel count builds a new one.
	switch {
	case d.silk == nil || d.silkChan != coded:
		dec, err := silk.NewDecoder(silk.Config{
			SampleRateKHz: rate,
			FrameMS:       frameMS,
			Channels:      coded,
		})
		if err != nil {
			return nil, err
		}
		d.silk, d.silkRate, d.silkChan = dec, rate, coded
	case d.silkRate != rate:
		d.silk.SetRate(rate)
		d.silkRate = rate
	}

	pcm, err := d.silk.Decode(dec, frameMS)
	if err != nil {
		return nil, err
	}

	// Delivered channels, not coded ones. A packet that codes mono in a stereo stream has its
	// single channel sent to both, which is what keeps the two in step when the stream changes.
	out := make([][]float32, d.channels)
	for c := range out {
		src := pcm[min(c, len(pcm)-1)]
		out[c] = make([]float32, len(src))
		for i, v := range src {
			out[c][i] = float32(v) / 32768
		}
	}
	return out, nil
}

// redundancy describes the short frame of the other codec a packet may carry alongside its own.
//
// A stream that changes codec, bandwidth or frame size puts a five millisecond frame of the outgoing
// codec at the end of the packet, so that the decoder can cross-fade rather than cut. Its bytes are
// not the main frame's, and its entropy decoder is separate.
type redundancy struct {
	present    bool
	celtToSilk bool
	bytes      int
	// length is what remains of the frame for the main decode.
	length int
}

// readRedundancy reads the flags that sit between SILK's symbols and CELT's.
//
// They are only present where there is room for them, which is why the position is tested before
// anything is read. A CELT-only packet never carries them at all.
func readRedundancy(dec *rangecoder.Decoder, length int, hybrid bool) redundancy {
	r := redundancy{length: length}

	need := 17
	if hybrid {
		need += 20
	}
	if dec.Tell()+need > 8*length {
		return r
	}

	// Outside hybrid the flag is implied: there is redundancy whenever there was room to say so.
	r.present = true
	if hybrid {
		r.present = dec.DecodeBitLogp(12) != 0
	}
	if !r.present {
		return r
	}

	r.celtToSilk = dec.DecodeBitLogp(1) != 0
	if hybrid {
		r.bytes = int(dec.DecodeUint(256)) + 2
	} else {
		r.bytes = length - (dec.Tell()+7)>>3
	}

	if r.bytes < 0 || length-r.bytes < 0 || (length-r.bytes)*8 < dec.Tell() {
		// Not a shape a valid packet takes; leaving the frame whole is the safe reading.
		return redundancy{length: length}
	}
	// The raw bits the main frame reads backwards must stop before the redundant frame's bytes.
	dec.Shrink(r.bytes)
	r.length = length - r.bytes
	return r
}

// redundantSamples is the length of a redundant frame, and fadeSamples half of it: the first half is
// played outright and the second is faded across.
const (
	redundantSamples = 240 // 5 ms at 48 kHz
	fadeSamples      = 120
)

// decodeRedundant decodes the short frame carried alongside the main one and returns its samples and
// its entropy coder state.
//
// The decoder it runs on is the same one the main frame uses, at band zero rather than at the hybrid
// start band. Going into SILK it keeps its state, because the frame it produces continues what came
// before; coming out of SILK it starts clean, because nothing preceded it.
func (d *Decoder) decodeRedundant(p *Packet, frame []byte, r redundancy, coded int) ([][]float32, uint32, error) {
	if r.bytes < 2 {
		return nil, 0, nil
	}
	data := frame[len(frame)-r.bytes:]

	if !r.celtToSilk {
		d.celt = nil
	}
	dec := rangecoder.NewDecoder(data)
	pcm, err := d.decodeCELT(p, dec, len(data), redundantSamples, coded, 0)
	if err != nil {
		return nil, 0, err
	}
	return pcm, dec.Range(), nil
}

// window returns the overlap window the cross-fade shapes itself with, which is the transform
// codec's own.
func (d *Decoder) window() []float32 { return celt.Window(celt.Overlap) }

// applyRedundancy decodes and blends the redundant frame of a packet that carries no CELT of its
// own, and records the entropy state the two together produce.
func (d *Decoder) applyRedundancy(p *Packet, frame []byte, red redundancy,
	out [][]float32, coded int, dec *rangecoder.Decoder,
) error {
	mainRange := dec.Range()
	if !red.present {
		d.finalRange = mainRange
		return nil
	}

	early, redRange, err := d.decodeRedundant(p, frame, red, coded)
	if err != nil {
		return err
	}
	d.blendRedundancy(out, early, red, len(out[0]))
	d.prevRedundancy = !red.celtToSilk

	// The reference reports the two ranges combined, so that a decoder which skipped the redundant
	// frame cannot pass for one that read it.
	d.finalRange = mainRange ^ redRange
	return nil
}

// blendRedundancy puts the redundant frame where the transition it covers happens.
//
// Coming out of the transform codec it belongs at the start, replacing the first half outright and
// fading across the second; going into it, at the end, fading the other way.
func (d *Decoder) blendRedundancy(out, early [][]float32, red redundancy, samples int) {
	if early == nil || len(out) == 0 || samples < redundantSamples {
		return
	}
	window := d.window()

	if red.celtToSilk {
		for c := range out {
			copy(out[c][:fadeSamples], early[c][:fadeSamples])
		}
		tail := make([][]float32, len(early))
		for c := range early {
			tail[c] = early[c][fadeSamples:]
		}
		// The redundant frame fades out as the main one fades in.
		fadeInto(out, tail, fadeSamples, window)
		return
	}
	tail := make([][]float32, len(early))
	for c := range early {
		tail[c] = early[c][fadeSamples:]
	}
	fadeInto(out, tail, samples-fadeSamples, window)
}

// fadeInto mixes to[0:fadeSamples] over out starting at position at.
func fadeInto(out, to [][]float32, at int, window []float32) {
	for c := range out {
		for i := range fadeSamples {
			w := window[i] * window[i]
			out[c][at+i] = w*to[c][i] + (1-w)*out[c][at+i]
		}
	}
}
