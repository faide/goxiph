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
	// The current packet's configuration, kept because a packet that never arrives still has to be
	// concealed at the width, channel count and frame size the stream was running at. They are the
	// packet's rather than the frame's, and are set before any frame of it is decoded.
	bandwidth    Bandwidth
	coded        int
	frameSamples int

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
	// Before any frame is decoded, because the cross-fade a mode change asks for is concealed at
	// the incoming packet's width even though it plays the outgoing codec.
	d.bandwidth, d.coded, d.frameSamples = p.Bandwidth, coded, frameSamples

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

// maxCELTSamples is the longest the transform codec conceals at once, so a longer gap is concealed
// in pieces. silenceSamples is the short block a frame leaving hybrid mode runs to fade its overlap.
const (
	maxCELTSamples = 960
	silenceSamples = 120
)

// decodeFrame decodes one frame of a packet with whichever codec its configuration names.
//
// The order below is the reference's, and most of it matters: the linear predictor leaves the
// entropy decoder where the transform codec starts reading, the cross-fade has to be generated
// before this packet touches either codec's state, and a codec is not restarted until the frames
// that need its old state have been produced.
func (d *Decoder) decodeFrame(p *Packet, frame []byte, samples, coded int) ([][]float32, error) {
	// A frame of one byte or none carries no symbols. The reference conceals it rather than playing
	// silence, so that a stream which stops sending during a pause fades out instead of cutting.
	if len(frame) <= 1 {
		return d.Conceal(samples)
	}

	prevMode, hadPrev, prevRedundancy := d.prevMode, d.hasPrev, d.prevRedundancy

	// Moving into or out of the transform codec alone is heard as a step, because the two codecs
	// leave nothing in common behind them. The outgoing one is asked what it would have played and
	// the two are cross-faded. Redundancy covers the same ground with a real frame rather than an
	// extrapolated one, so where a packet carries it this is not done.
	transition := hadPrev &&
		((p.Mode == ModeCELT && prevMode != ModeCELT && !prevRedundancy) ||
			(p.Mode != ModeCELT && prevMode == ModeCELT))

	var bridge [][]float32
	var err error
	if transition && p.Mode == ModeCELT {
		if bridge, err = d.Conceal(min(redundantSamples, samples)); err != nil {
			return nil, err
		}
	}

	dec := rangecoder.NewDecoder(frame)
	red := redundancy{length: len(frame)}
	var low [][]float32

	if p.Mode != ModeCELT {
		// Coming back from the transform codec the predictor has missed however long that lasted,
		// and nothing it holds describes the signal it is about to continue.
		if hadPrev && prevMode == ModeCELT {
			d.silk = nil
		}
		rate := silkRateFor(p.Bandwidth)
		if p.Mode == ModeHybrid {
			rate = hybridSILKRate
		}
		if low, err = d.decodeSILK(p, dec, samples, coded, rate); err != nil {
			return nil, err
		}
		red = readRedundancy(dec, len(frame), p.Mode == ModeHybrid)
	}

	if red.present {
		transition = false
	}
	if transition && p.Mode != ModeCELT {
		// The other direction, generated here rather than above because the predictor's own decode
		// has to come first: this call conceals with the transform codec, which reads no symbols.
		if bridge, err = d.Conceal(min(redundantSamples, samples)); err != nil {
			return nil, err
		}
	}

	// Going into the predictor the redundant frame is read first, because the main frame's decode
	// continues from the state it leaves.
	var early [][]float32
	var redRange uint32
	if red.present && red.celtToSilk {
		if early, redRange, err = d.decodeRedundant(p, frame, red, coded); err != nil {
			return nil, err
		}
	}

	start := hybridStartBand
	if p.Mode == ModeCELT {
		start = 0
	}

	var out [][]float32
	if p.Mode == ModeSILK {
		out = d.silence(samples)
		// A frame that follows a hybrid one lets the transform codec's overlap decay instead of
		// cutting it, by running one short block of silence through the transform.
		bridged := red.present && red.celtToSilk && prevRedundancy
		if prevMode == ModeHybrid && !bridged {
			if err := d.celtSilence(p, coded, out); err != nil {
				return nil, err
			}
		}
	} else {
		// Only now, once every frame that needed the old state has been produced.
		if hadPrev && p.Mode != prevMode && !prevRedundancy {
			d.celt = nil
		}
		if out, err = d.decodeCELT(p, dec, red.length, samples, coded, start); err != nil {
			return nil, err
		}
	}
	mainRange := dec.Range()

	// The two codecs cover different bands of the same signal, so the result is their sum.
	if p.Mode != ModeCELT {
		for c := range out {
			for i := range out[c] {
				if i < len(low[c]) {
					out[c][i] += low[c][i]
				}
			}
		}
	}

	if red.present && !red.celtToSilk {
		if early, redRange, err = d.decodeRedundant(p, frame, red, coded); err != nil {
			return nil, err
		}
	}
	d.blendRedundancy(out, early, red, samples)
	if transition && bridge != nil {
		d.blendTransition(out, bridge, samples)
	}

	d.prevMode, d.hasPrev = p.Mode, true
	d.prevRedundancy = red.present && !red.celtToSilk
	// The reference reports the two ranges combined, so that a decoder which skipped the redundant
	// frame cannot pass for one that read it.
	d.finalRange = mainRange ^ redRange
	return out, nil
}

// silence returns one frame of nothing, which is what a decoder with no history can offer.
func (d *Decoder) silence(samples int) [][]float32 {
	out := make([][]float32, d.channels)
	for c := range out {
		out[c] = make([]float32, samples)
	}
	return out
}

// celtSilence runs the shortest transform block on a frame that codes nothing, writing its output
// over the start of out.
//
// The two bytes are the smallest payload the entropy decoder accepts. Nothing is coded in them; what
// the block produces is the overlap the previous frame left, windowed out.
func (d *Decoder) celtSilence(p *Packet, coded int, out [][]float32) error {
	if d.celt == nil {
		return nil
	}
	silence := []byte{0xFF, 0xFF}
	pcm, err := d.decodeCELT(p, rangecoder.NewDecoder(silence), len(silence), silenceSamples, coded, 0)
	if err != nil {
		return err
	}
	for c := range out {
		copy(out[c], pcm[c])
	}
	return nil
}

// Conceal produces the samples of a packet that never arrived.
//
// The duration is the caller's to give: Opus carries no length for a packet a decoder never saw, and
// a container knows how long the gap is where the codec cannot. It is capped at one frame of
// whatever the last packet ran at, because that is as much as the extrapolation is worth; a caller
// with a longer gap to fill calls again.
func (d *Decoder) Conceal(samples int) ([][]float32, error) {
	if samples <= 0 {
		return nil, fmt.Errorf("opus: %d samples to conceal", samples)
	}
	d.finalRange = 0
	if !d.hasPrev {
		return d.silence(samples), nil
	}
	samples = min(samples, d.frameSamples)

	// The transform codec conceals one frame at a time, so a longer gap goes in twenty millisecond
	// pieces. The predictor has no such limit and takes the gap whole.
	step := samples
	if d.prevMode != ModeSILK && samples > maxCELTSamples {
		step = maxCELTSamples
	}

	out := make([][]float32, d.channels)
	for at := 0; at < samples; at += step {
		got, err := d.concealFrame(min(step, samples-at))
		if err != nil {
			return nil, err
		}
		for c := range out {
			out[c] = append(out[c], got[c]...)
		}
	}
	return out, nil
}

// concealFrame extrapolates one frame from whichever codecs were running when the stream stopped.
func (d *Decoder) concealFrame(samples int) ([][]float32, error) {
	out := d.silence(samples)
	d.prevRedundancy = false

	if d.prevMode != ModeCELT && d.silk != nil {
		// The predictor conceals no less than ten milliseconds, so a shorter gap takes the start of
		// a longer concealed frame.
		pcm, err := d.silk.Conceal(max(10, samples/(OutputRate/1000)))
		if err != nil {
			return nil, err
		}
		for c := range out {
			src := pcm[min(c, len(pcm)-1)]
			for i := range out[c] {
				out[c][i] += float32(src[i]) / 32768
			}
		}
	}

	if d.prevMode != ModeSILK && d.celt != nil {
		if err := d.celt.Configure(d.coded, start(d.prevMode), endBandFor(d.bandwidth)); err != nil {
			return nil, err
		}
		size, err := celt.FrameSizeForSamples(min(samples, maxCELTSamples))
		if err != nil {
			return nil, err
		}
		high := d.celt.Conceal(size)
		for c := range out {
			for i := range out[c] {
				out[c][i] += high[c][i]
			}
		}
	}
	return out, nil
}

// start is the first CELT band a mode codes.
func start(m Mode) int {
	if m == ModeCELT {
		return 0
	}
	return hybridStartBand
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
	// Nothing here builds a new decoder once one exists. A change of packet duration, of internal
	// rate or of channel count each keeps as much of the state as still means anything, because a
	// stream moves between them mid-sentence and a decoder that started over would be heard.
	if d.silk == nil {
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
	// Both of these keep what they can. A stream that gains a channel keeps the one it had, and a
	// change of rate keeps what the entropy decoding refers back to.
	if d.silkChan != coded {
		d.silk.SetChannels(coded)
		d.silkChan = coded
	}
	if d.silkRate != rate {
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

// blendRedundancy puts the redundant frame where the transition it covers happens.
//
// Coming out of the transform codec it belongs at the start, replacing the first half outright and
// fading across the second; going into it, at the end, fading the other way.
func (d *Decoder) blendRedundancy(out, early [][]float32, red redundancy, samples int) {

	if early == nil || len(out) == 0 || samples < redundantSamples {
		return
	}
	window := d.window()

	tail := tailAt(early, fadeSamples)

	if red.celtToSilk {
		for c := range out {
			copy(out[c][:fadeSamples], early[c][:fadeSamples])
		}
		// The redundant frame fades out as the main one fades in.
		fadeFrom(out, tail, fadeSamples, window)
		return
	}
	fadeInto(out, tail, samples-fadeSamples, window)
}

// blendTransition puts the cross-fade across a change of codec at the start of the frame.
//
// The bridge is what the outgoing codec would have played, so the frame begins on it and crosses to
// what the incoming one decoded. A frame too short to hold both halves crosses over its whole
// length, which does not preserve the level exactly but is better than a step.
func (d *Decoder) blendTransition(out, bridge [][]float32, samples int) {
	window := d.window()
	if samples < redundantSamples {
		fadeFrom(out, bridge, 0, window)
		return
	}
	for c := range out {
		copy(out[c][:fadeSamples], bridge[c][:fadeSamples])
	}
	fadeFrom(out, tailAt(bridge, fadeSamples), fadeSamples, window)
}

// tailAt returns each channel from position at onwards.
func tailAt(v [][]float32, at int) [][]float32 {
	out := make([][]float32, len(v))
	for c := range v {
		out[c] = v[c][at:]
	}
	return out
}

// fadeInto and fadeFrom cross-fade fadeSamples of out at position at, on the squared window so that
// the two sides sum to one in power. fadeInto ends on other and fadeFrom begins on it.
func fadeInto(out, other [][]float32, at int, window []float32) {
	for c := range out {
		for i := range fadeSamples {
			w := window[i] * window[i]
			out[c][at+i] = w*other[c][i] + (1-w)*out[c][at+i]
		}
	}
}

func fadeFrom(out, other [][]float32, at int, window []float32) {
	for c := range out {
		for i := range fadeSamples {
			w := window[i] * window[i]
			out[c][at+i] = w*out[c][at+i] + (1-w)*other[c][i]
		}
	}
}
