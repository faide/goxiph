package celt

import "math"

// The band decoder leaves each band as a unit-norm shape, with its level held separately as a log
// energy. Putting the two back together is the last step before the transform.
//
// Adapted from celt/bands.c and celt/quant_bands.c.

// eMeansQ6 is the mean energy of each band, as the fixed-point reference stores it.
//
// The coarse energy is coded as a departure from these, so they have to be added back before the
// value means anything. The reference ships a second copy pre-divided as floats; keeping the
// integers and dividing here means one transcription instead of two, and the conformance test
// checks the division against that float copy.
var eMeansQ6 = [25]int8{
	103, 100, 92, 85, 81,
	77, 72, 70, 78, 75,
	73, 71, 78, 74, 69,
	72, 70, 74, 76, 71,
	60, 60, 60, 60, 60,
}

// eMean returns a band's mean energy in the log domain.
func eMean(band int) float32 { return float32(eMeansQ6[band]) / 16 }

// LogToAmplitude converts decoded log-domain band energies into linear amplitudes.
//
// Bands outside the coded range come out at zero rather than at their mean, because a band that was
// never coded carries no signal at all.
func LogToAmplitude(amp, logE []float32, start, end int) {
	for i := range NumBands {
		if i < start || i >= end {
			amp[i] = 0
			continue
		}
		amp[i] = float32(math.Exp2(float64(logE[i] + eMean(i))))
	}
}

// DenormaliseBands scales each band's shape by its amplitude, writing the MDCT spectrum.
//
// Everything above the last coded band is zeroed: the bands stop at 20 kHz and the bins beyond carry
// nothing, so leaving them would put whatever the buffer held into the output.
func DenormaliseBands(freq, x, amp []float32, end, lm int) {
	m := 1 << uint(lm)
	for i := range end {
		g := amp[i]
		for j := m * BandEdges[i]; j < m*BandEdges[i+1]; j++ {
			freq[j] = x[j] * g
		}
	}
	clear(freq[m*BandEdges[end]:])
}

// AntiCollapse refills the time blocks a band lost to quantisation.
//
// A band given few bits may put all its pulses in one block and leave the others empty. Across a
// transient that reads as a gap rather than as quiet, so the empty blocks are filled with noise at a
// level set by how far the band's energy has fallen since the previous frames: a band that just got
// much louder gets less, because its own signal already masks the gap.
//
// x holds one slice per channel and masks are interleaved by channel, as the band loop writes them.
// The previous-frame energies always carry two channels, even in mono, because a mono frame reads
// the second slot to cover a stream that has just changed channel count.
func AntiCollapse(x [][]float32, masks []byte, logE, prev1, prev2 [][]float32,
	pulses []int, lm, start, end int, seed uint32,
) uint32 {
	channels := len(x)
	m := 1 << uint(lm)

	for i := start; i < end; i++ {
		width := BandEdges[i+1] - BandEdges[i]

		// Bits per sample in eighths, which is how much detail the band was given.
		depth := (1 + pulses[i]) / (width << uint(lm))
		thresh := 0.5 * math.Exp2(-0.125*float64(depth))
		invSqrtN := 1 / math.Sqrt(float64(width<<uint(lm)))

		for c := range channels {
			p1, p2 := prev1[c][i], prev2[c][i]
			if channels == 1 {
				p1 = max(p1, prev1[1][i])
				p2 = max(p2, prev2[1][i])
			}

			eDiff := float64(logE[c][i] - min(p1, p2))
			eDiff = max(0, eDiff)

			// Short blocks carry less energy each, so the fill is scaled up to match.
			r := 2 * math.Exp2(-eDiff)
			if lm == 3 {
				r *= math.Sqrt2
			}
			r = min(thresh, r) * invSqrtN

			band := x[c][BandEdges[i]<<uint(lm) : BandEdges[i+1]<<uint(lm)]
			refill := false
			for k := range m {
				if masks[i*channels+c]&(1<<uint(k)) != 0 {
					continue
				}
				for j := range width {
					seed = lcgRand(seed)
					if seed&0x8000 != 0 {
						band[(j<<uint(lm))+k] = float32(r)
					} else {
						band[(j<<uint(lm))+k] = float32(-r)
					}
				}
				refill = true
			}
			if refill {
				renormalise(band, 1)
			}
		}
	}
	return seed
}
