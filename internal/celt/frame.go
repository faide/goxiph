package celt

import (
	"fmt"

	"github.com/faide/goxiph/internal/rangecoder"
)

// A CELT frame carries its parameters in a fixed order, each one conditional on there being room
// left for it. That ordering is the format: a decoder that reads a symbol the encoder chose to omit
// consumes the wrong bits and everything after it is noise.
//
// Adapted from celt/celt.c.

// ShortMdctSize is the length of one short block at 48 kHz, and the unit the frame length is built
// from. A frame holds this many bins per block.
const ShortMdctSize = 120

// silenceEnergy is the log energy a band takes when the frame has none, from celt/celt.c.
const silenceEnergy = -28

// tapsetICDF is the distribution of the post-filter tap set, from celt/celt.c.
var tapsetICDF = []byte{2, 1, 0}

// PostFilter is the pitch filter a frame may ask for, to sharpen periodic signals that the
// transform alone smears.
type PostFilter struct {
	Pitch  int
	Gain   float32
	Tapset int
}

// Decoder decodes CELT frames and carries the state that spans them.
//
// The band energies are kept for three frames: the current one predicts from the previous, and the
// anti-collapse fill measures against the two before it. Both are held for two channels whatever the
// stream carries, because a mono frame reads the second slot and the channel count can change.
type Decoder struct {
	// channels is what the decoder delivers, fixed for the stream; streamChannels is what the
	// current packet codes, which may be fewer. The two are separate because the synthesis always
	// runs on the delivered count: a stream that drops to mono keeps both filter chains alive, so
	// that going back to stereo resumes rather than restarts.
	channels       int
	streamChannels int
	start, end     int

	logE  [2][]float32 // the current frame's band energies, in the log domain
	prev1 [2][]float32 // the previous frame's
	prev2 [2][]float32 // the one before that

	rng uint32

	mdct *MDCT
	// decodeMem holds each channel's output history followed by its overlap tail. The history is
	// what the post-filter reaches back into for the pitch period.
	decodeMem [][]float32
	preemph   []float32
	// pf is the previous frame's post-filter and pfOld the one before it. The transform's output
	// spans both, so the filter cross-fades across two frames rather than switching at a boundary.
	pf, pfOld PostFilter
}

// NewDecoder returns a decoder for the given channel count and band range.
func NewDecoder(channels, start, end int) (*Decoder, error) {
	if channels != 1 && channels != 2 {
		return nil, fmt.Errorf("celt: %d channels", channels)
	}
	if start < 0 || end > NumBands || start >= end {
		return nil, fmt.Errorf("celt: band range %d to %d", start, end)
	}

	// The buffers are sized for two channels whatever this stream carries, because the count can
	// change from one packet to the next and the state has to survive that.
	d := &Decoder{
		channels:       channels,
		streamChannels: channels,
		start:          start,
		end:            end,
		mdct:           NewMDCT(ShortMdctSize << maxLM),
		decodeMem:      make([][]float32, 2),
		preemph:        make([]float32, 2),
	}
	for c := range 2 {
		d.decodeMem[c] = make([]float32, decodeBufferSize+Overlap)
	}
	for c := range 2 {
		d.logE[c] = make([]float32, NumBands)
		d.prev1[c] = make([]float32, NumBands)
		d.prev2[c] = make([]float32, NumBands)
		for i := range NumBands {
			d.prev1[c][i] = silenceEnergy
			d.prev2[c][i] = silenceEnergy
		}
	}
	return d, nil
}

// Configure moves the band range and channel count without disturbing anything else.
//
// A stream changes bandwidth, and moves between the transform codec alone and the transform codec
// atop the linear predictor, without any of it being a fresh start: the band energies, the filter
// state and the overlap all carry across. Rebuilding the decoder instead would reset the energy
// prediction, which is heard as a step at every change and shows in no symbol.
func (d *Decoder) Configure(streamChannels, start, end int) error {
	if streamChannels != 1 && streamChannels != 2 {
		return fmt.Errorf("celt: %d coded channels", streamChannels)
	}
	if start < 0 || end > NumBands || start >= end {
		return fmt.Errorf("celt: band range %d to %d", start, end)
	}
	d.streamChannels, d.start, d.end = streamChannels, start, end
	return nil
}

// Range returns the entropy coder state after the last frame.
//
// This is the value `opusdec --save-range` reports. It is a checksum of every symbol read, so two
// decoders agree on it only if they read the same symbols in the same order.
func (d *Decoder) Range() uint32 { return d.rng }

// Frame is one decoded CELT frame.
type Frame struct {
	// Spectrum holds the denormalised MDCT bins, one slice per channel.
	Spectrum [][]float32
	// Transient reports that the frame was coded as short blocks.
	Transient bool
	// PCM holds the decoded samples, one slice per channel, in the range minus one to one.
	PCM [][]float32
	// PostFilter is the pitch filter the frame asked for, if any.
	PostFilter PostFilter
	// Silence reports that the frame carries nothing.
	Silence bool
}

// DecodeFrame reads one CELT frame of the given size from a range decoder positioned at its start.
//
// length is the frame's size in bytes, which bounds every conditional read in it.
func (d *Decoder) DecodeFrame(dec *rangecoder.Decoder, length int, frame FrameSize) (*Frame, error) {
	lm := int(frame)
	m := 1 << uint(lm)
	n := m * ShortMdctSize
	c := d.streamChannels

	// One spectrum per delivered channel, though only the coded ones are read from the stream.
	out := &Frame{Spectrum: make([][]float32, d.channels)}
	for i := range out.Spectrum {
		out.Spectrum[i] = make([]float32, n)
	}

	// A stream that has just dropped to mono inherits whichever channel was louder.
	if c == 1 {
		for i := range NumBands {
			d.logE[0][i] = max(d.logE[0][i], d.logE[1][i])
		}
	}

	totalBits := length * 8
	tell := dec.Tell()

	// Silence is the one flag that can be implied rather than coded: a frame with nothing left to
	// read is silent by exhaustion.
	silence := false
	switch {
	case tell >= totalBits:
		silence = true
	case tell == 1:
		silence = dec.DecodeBitLogp(15) != 0
	}
	if silence {
		dec.SkipTo(totalBits)
		tell = totalBits
	}

	var pf PostFilter
	if d.start == 0 && tell+16 <= totalBits {
		if dec.DecodeBitLogp(1) != 0 {
			// The pitch is coded as an octave and an offset within it, which spends resolution where
			// the ear has it.
			octave := int(dec.DecodeUint(6))
			pf.Pitch = (16 << uint(octave)) + int(dec.DecodeBits(uint(4+octave))) - 1
			qg := int(dec.DecodeBits(3))
			if dec.Tell()+2 <= totalBits {
				pf.Tapset = dec.DecodeICDF(tapsetICDF, 2)
			}
			pf.Gain = 0.09375 * float32(qg+1)
		}
		tell = dec.Tell()
	}

	transient := false
	if lm > 0 && tell+3 <= totalBits {
		transient = dec.DecodeBitLogp(3) != 0
		tell = dec.Tell()
	}

	intra := tell+3 <= totalBits && dec.DecodeBitLogp(3) != 0

	DecodeCoarseEnergy(dec, d.logE[:c], d.start, d.end, frame, intra, totalBits)

	tf := make([]int, NumBands)
	DecodeTFResolution(dec, tf, d.start, d.end, lm, transient, totalBits)
	spread := DecodeSpread(dec, totalBits)

	caps := Caps(frame, c)
	var offsets [NumBands]int
	totalFrac := totalBits << BitRes
	totalBoost := DecodeBoosts(dec, &offsets, d.start, d.end, frame, c, &caps, totalFrac)
	trim := DecodeTrim(dec, totalFrac, totalBoost)

	// One bit is held back for the anti-collapse flag, but only where a frame could collapse: that
	// needs short blocks, and enough of them for a block to come out empty.
	bits := totalFrac - dec.TellFrac() - 1
	antiCollapseRsv := 0
	if transient && lm >= 2 && bits >= (lm+2)<<BitRes {
		antiCollapseRsv = 1 << BitRes
	}
	bits -= antiCollapseRsv

	alloc := ComputeAllocation(dec, d.start, d.end, &offsets, &caps, trim, bits, frame, c, transient)
	DecodeFineEnergy(dec, d.logE[:c], d.start, d.end, alloc.FineBits[:])

	masks := make([]byte, NumBands*c)
	bp := &BandParams{
		Start: d.start, End: d.end,
		X:             out.Spectrum[0],
		CollapseMasks: masks,
		Pulses:        alloc.Pulses[:],
		TFRes:         tf,
		ShortBlocks:   transient,
		Spread:        spread,
		DualStereo:    alloc.DualStereo,
		Intensity:     alloc.Intensity,
		TotalBits:     totalFrac - antiCollapseRsv,
		Balance:       alloc.Balance,
		LM:            lm,
		CodedBands:    alloc.CodedBands,
		Seed:          d.rng,
	}
	if c == 2 {
		bp.Y = out.Spectrum[1]
	}
	d.rng = DecodeBands(dec, bp)

	antiCollapseOn := antiCollapseRsv > 0 && dec.DecodeBits(1) != 0

	DecodeFinalEnergy(dec, d.logE[:c], d.start, d.end,
		alloc.FineBits[:], alloc.FinePriority[:], totalBits-dec.Tell())

	if antiCollapseOn {
		d.rng = AntiCollapse(out.Spectrum[:c], masks, d.logE[:c], d.prev1[:], d.prev2[:],
			alloc.Pulses[:], lm, d.start, d.end, d.rng)
	}

	amp := make([]float32, NumBands)
	for ch := range c {
		LogToAmplitude(amp, d.logE[ch], d.start, d.end)
		if silence {
			clear(amp)
		}
		DenormaliseBands(out.Spectrum[ch], out.Spectrum[ch], amp, d.end, lm)
	}

	// Reconcile what was coded with what is delivered, before the transform rather than after it.
	// Both filter chains then run on a signal every frame, so neither goes stale.
	switch {
	case d.channels == 2 && c == 1:
		copy(out.Spectrum[1], out.Spectrum[0])
	case d.channels == 1 && c == 2:
		for i := range out.Spectrum[0] {
			out.Spectrum[0][i] = 0.5 * (out.Spectrum[0][i] + out.Spectrum[1][i])
		}
	}

	d.synthesise(out, n, lm, transient, pf)

	d.advanceHistory(transient)
	// The range coder's own state seeds the next frame's noise, and is what a reference decode
	// reports for comparison.
	d.rng = dec.Range()

	out.Transient = transient
	out.PostFilter = pf
	out.Silence = silence

	if dec.Tell() > totalBits {
		return out, fmt.Errorf("celt: frame read %d bits of %d", dec.Tell(), totalBits)
	}
	return out, nil
}

// advanceHistory rolls the band energies forward.
//
// A transient frame does not become the new history outright; it only lowers it. Its energies are
// measured over short blocks and would otherwise make the next frame predict from a peak.
func (d *Decoder) advanceHistory(transient bool) {
	// The coded count, not the delivered one: a mono packet leaves the second channel's energies
	// standing in for its own, so that a later stereo packet predicts from something.
	if d.streamChannels == 1 {
		copy(d.logE[1], d.logE[0])
	}

	for c := range 2 {
		if transient {
			for i := range NumBands {
				d.prev1[c][i] = min(d.prev1[c][i], d.logE[c][i])
			}
		} else {
			copy(d.prev2[c], d.prev1[c])
			copy(d.prev1[c], d.logE[c])
		}

		// Bands outside the coded range hold nothing, and must not be predicted from.
		for i := range NumBands {
			if i >= d.start && i < d.end {
				continue
			}
			d.logE[c][i] = 0
			d.prev1[c][i] = silenceEnergy
			d.prev2[c][i] = silenceEnergy
		}
	}
}

// synthesise turns the frame's spectrum into samples.
//
// The transform's output overlaps the previous frame, so it is added into a running history rather
// than emitted on its own; the post-filter then works on that history, where the pitch period it
// reaches back for is still available.
func (d *Decoder) synthesise(out *Frame, n, lm int, transient bool, pf PostFilter) {
	out.PCM = make([][]float32, d.channels)
	block := make([]float32, n+Overlap)

	for c := range d.channels {
		mem := d.decodeMem[c]
		// Make room at the end of the history for this frame.
		copy(mem[:decodeBufferSize-n], mem[n:decodeBufferSize])

		d.mdct.InverseFrame(out.Spectrum[c], block, lm, transient, maxLM)

		// The leading overlap completes the previous frame's tail; the trailing one waits for the next.
		syn := decodeBufferSize - n
		overlapMem := mem[decodeBufferSize:]
		for j := range Overlap {
			mem[syn+j] = block[j] + overlapMem[j]
		}
		copy(mem[syn+Overlap:decodeBufferSize], block[Overlap:n])
		copy(overlapMem, block[n:n+Overlap])

		// The filter needs a period it can reach back for, whatever was signalled.
		periodOld := max(d.pfOld.Pitch, combMinPeriod)
		period := max(d.pf.Pitch, combMinPeriod)
		window := d.mdct.Window()

		combFilter(mem, syn, periodOld, period, ShortMdctSize,
			d.pfOld.Gain, d.pf.Gain, d.pfOld.Tapset, d.pf.Tapset, window, Overlap)
		if lm != 0 {
			combFilter(mem, syn+ShortMdctSize, period, max(pf.Pitch, combMinPeriod),
				n-ShortMdctSize, d.pf.Gain, pf.Gain, d.pf.Tapset, pf.Tapset, window, Overlap)
		}

		out.PCM[c] = make([]float32, n)
		deemphasis(out.PCM[c], mem, syn, n, &d.preemph[c])
	}

	// A frame longer than one short block has already cross-faded to the new filter above, so both
	// slots move on together; a shortest frame has not, and keeps one frame of lag.
	d.pfOld = d.pf
	d.pf = pf
	if lm != 0 {
		d.pfOld = pf
	}
}
