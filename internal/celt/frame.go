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
	channels   int
	start, end int

	logE  [2][]float32 // the current frame's band energies, in the log domain
	prev1 [2][]float32 // the previous frame's
	prev2 [2][]float32 // the one before that

	rng uint32
}

// NewDecoder returns a decoder for the given channel count and band range.
func NewDecoder(channels, start, end int) (*Decoder, error) {
	if channels != 1 && channels != 2 {
		return nil, fmt.Errorf("celt: %d channels", channels)
	}
	if start < 0 || end > NumBands || start >= end {
		return nil, fmt.Errorf("celt: band range %d to %d", start, end)
	}

	d := &Decoder{channels: channels, start: start, end: end}
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
	c := d.channels

	out := &Frame{Spectrum: make([][]float32, c)}
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
	totalBoost := DecodeBoosts(dec, &offsets, d.start, d.end, frame, &caps, totalFrac)
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
		d.rng = AntiCollapse(out.Spectrum, masks, d.logE[:c], d.prev1[:], d.prev2[:],
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
	if d.channels == 1 {
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
