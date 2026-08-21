package celt

import "math"

// A lost packet still has to produce samples, and silence is the worst of the options: it is heard
// as a hole. Concealment extends what came before instead — repeating the signal at its own pitch
// where it has one, and filling with shaped noise where it does not — and fades as the loss runs on,
// because a repeated waveform stops sounding like speech within a few frames.
//
// Adapted from celt/celt.c.

// pitchSearchOffset is how far back concealment looks for a period, and pitchSearchMin the shortest
// it will accept. They correspond to about 67 Hz and 480 Hz.
const (
	pitchSearchOffset = 720
	pitchSearchMin    = 100
)

// lossesBeforeNoise is how many consecutive losses turn concealment from repetition to noise.
//
// Repeating a waveform for longer than this stops sounding like the signal and starts sounding like
// a tone, so the fill moves to noise at the background level instead.
const lossesBeforeNoise = 5

// Conceal produces one frame without a packet, and returns its samples.
//
// It is used for outright loss and for the cross-fade across a change of codec, where the outgoing
// codec is asked what it would have played.
func (d *Decoder) Conceal(frame FrameSize) [][]float32 {
	lm := int(frame)
	n := ShortMdctSize << uint(lm)

	if d.lossCount >= lossesBeforeNoise || d.start != 0 {
		d.concealWithNoise(n, lm)
	} else {
		d.concealWithPitch(n, lm)
	}

	out := make([][]float32, d.channels)
	syn := decodeBufferSize - n
	for c := range d.channels {
		out[c] = make([]float32, n)
		deemphasis(out[c], d.decodeMem[c], syn, n, &d.preemph[c])
	}
	d.lossCount++
	return out
}

// concealWithNoise fills the bands with noise at a level taken from the recent past.
//
// Every band keeps its own level, so the result has the spectrum of what was being played rather
// than the flat hiss that filling the whole frame at once would give.
func (d *Decoder) concealWithNoise(n, lm int) {
	amp := make([]float32, NumBands)
	spectrum := make([][]float32, d.channels)

	for c := range d.channels {
		if d.lossCount >= lossesBeforeNoise {
			LogToAmplitude(amp, d.background[c], d.start, d.end)
		} else {
			// The level falls as the loss runs on: a decibel and a half for the first frame, half a
			// decibel for each after it.
			decay := float32(0.5)
			if d.lossCount == 0 {
				decay = 1.5
			}
			for i := d.start; i < d.end; i++ {
				d.logE[c][i] -= decay
			}
			LogToAmplitude(amp, d.logE[c], d.start, d.end)
		}

		spectrum[c] = make([]float32, n)
		for b := d.start; b < NumBands; b++ {
			lo, hi := BandEdges[b]<<uint(lm), BandEdges[b+1]<<uint(lm)
			if b >= d.end {
				break
			}
			for j := lo; j < hi; j++ {
				d.rng = lcgRand(d.rng)
				spectrum[c][j] = float32(int32(d.rng) >> 20)
			}
			renormalise(spectrum[c][lo:hi], 1)
		}
		DenormaliseBands(spectrum[c], spectrum[c], amp, d.end, lm)
	}

	block := make([]float32, n+Overlap)
	for c := range d.channels {
		mem := d.decodeMem[c]
		copy(mem[:decodeBufferSize-n], mem[n:decodeBufferSize])
		d.mdct.InverseFrame(spectrum[c], block, lm, false, maxLM)

		syn := decodeBufferSize - n
		overlapMem := mem[decodeBufferSize:]
		for j := range Overlap {
			mem[syn+j] = block[j] + overlapMem[j]
		}
		copy(mem[syn+Overlap:decodeBufferSize], block[Overlap:n])
		copy(overlapMem, block[n:n+Overlap])
	}
}

// concealWithPitch extends the signal by repeating it at its own period.
//
// The repetition is of the excitation rather than of the waveform: the short-term filter is fitted
// once and re-applied, so the repeat carries the signal's spectral shape instead of copying whatever
// the filter happened to be doing at the cut.
func (d *Decoder) concealWithPitch(n, lm int) {
	fade := float32(1)
	if d.lossCount == 0 {
		lp := make([]float32, decodeBufferSize>>1)
		pitchDownsample(d.decodeMem[:d.channels], lp, decodeBufferSize)
		found := pitchSearch(lp[pitchSearchOffset>>1:], lp,
			decodeBufferSize-pitchSearchOffset, pitchSearchOffset-pitchSearchMin)
		d.lastPitch = pitchSearchOffset - found
	} else {
		// A repeat of a repeat is already suspect, so each one is quieter than the last.
		fade = 0.8
	}
	pitch := d.lastPitch

	length := n + Overlap
	window := d.mdct.Window()

	for c := range d.channels {
		// out is the recent output: the last period of it is what gets repeated.
		out := d.decodeMem[c][decodeBufferSize-combMaxPeriod:]
		exc := make([]float32, combMaxPeriod)
		copy(exc, out[:combMaxPeriod])

		if d.lossCount == 0 {
			ac := autocorrelate(exc, window, Overlap, lpcOrder)
			// The same noise floor and lag window the pitch analysis uses, for the same reason.
			ac[0] *= 1.0001
			for i := 1; i <= lpcOrder; i++ {
				ac[i] -= ac[i] * (0.008 * float64(i)) * (0.008 * float64(i))
			}
			d.lpc[c] = levinson(ac, lpcOrder)
		}

		mem := make([]float32, lpcOrder)
		for i := range lpcOrder {
			mem[i] = out[combMaxPeriod-1-i]
		}
		fir(exc, d.lpc[c], exc, lpcOrder, mem)

		// How fast the waveform was already dying away, so the repeat continues the trend rather
		// than holding a level the signal had left.
		decay := decayRate(exc, pitch)

		e := make([]float32, length+Overlap)
		offset := combMaxPeriod - pitch
		var s1 float64
		for i := range length + Overlap {
			if offset+i >= combMaxPeriod {
				offset -= pitch
				decay *= decay
			}
			e[i] = decay * exc[offset+i]
			t := float64(out[offset+i])
			s1 += t * t
		}

		for i := range lpcOrder {
			mem[i] = out[combMaxPeriod-1-i]
		}
		for i := range e {
			e[i] *= fade
		}
		iir(e, d.lpc[c], e, lpcOrder, mem)

		limitExplosion(e, s1)

		// The pitch filter is undone over the previous frame's tail and reapplied to the new one,
		// because the tail was stored with it already applied.
		combFilter(d.decodeMem[c], decodeBufferSize, d.pf.Pitch, d.pf.Pitch, Overlap,
			d.pf.Gain, d.pf.Gain, d.pf.Tapset, d.pf.Tapset, nil, 0)

		copy(out, out[n:combMaxPeriod+Overlap])

		// Fold the concealed frame's tail the way the transform would have, so the next frame's
		// overlap-add meets something shaped like a transform output.
		for i := range Overlap / 2 {
			t := window[i]*e[n+Overlap-1-i] + window[Overlap-i-1]*e[n+i]
			out[combMaxPeriod+i] = window[Overlap-i-1] * t
			out[combMaxPeriod+Overlap-i-1] = window[i] * t
		}
		copy(out[combMaxPeriod-n:combMaxPeriod], e[:n])

		// The tail is stored with the pitch filter applied, so the next frame's post-filter pass
		// meets what it expects. Undoing it here and writing the result back is what leaves it so.
		combFilterTo(e, 0, out, combMaxPeriod, d.pf.Pitch, d.pf.Pitch, Overlap,
			-d.pf.Gain, -d.pf.Gain, d.pf.Tapset, d.pf.Tapset, nil, 0)
		copy(out[combMaxPeriod:combMaxPeriod+Overlap], e[:Overlap])
	}
}

// decayRate compares the energy of the last period with the one before it, so that a signal already
// fading keeps fading.
func decayRate(exc []float32, pitch int) float32 {
	period := min(pitch, combMaxPeriod/2)
	e1, e2 := 1.0, 1.0
	for i := range period {
		a := float64(exc[combMaxPeriod-period+i])
		b := float64(exc[combMaxPeriod-2*period+i])
		e1 += a * a
		e2 += b * b
	}
	return float32(math.Sqrt(min(e1, e2) / e2))
}

// limitExplosion guards against the synthesis filter running away.
//
// A filter fitted to one stretch of signal and driven by another can resonate, and a concealed frame
// that grows without bound is far worse than one that is silent. The comparison is written so that a
// value that is not a number fails it too.
func limitExplosion(e []float32, s1 float64) {
	var s2 float64
	for _, v := range e {
		s2 += float64(v) * float64(v)
	}
	if !(s1 > 0.2*s2) {
		clear(e)
		return
	}
	if s1 < s2 {
		ratio := float32(math.Sqrt((s1 + 1) / (s2 + 1)))
		for i := range e {
			e[i] *= ratio
		}
	}
}
