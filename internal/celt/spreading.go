package celt

import (
	"math"

	"github.com/faide/goxiph/internal/rangecoder"
)

// Spreading rotates a band's shape before quantisation and back after it, so that a few large pulses
// become many small coefficients. Without it a sparsely allocated band collapses to a handful of
// tones, which is audible as a metallic ring on noise-like signals.
//
// The rotation is a chain of Givens rotations, so it is orthogonal: it preserves the unit norm the
// shape carries, and the decoder undoes it by rotating the other way.
//
// Adapted from celt/vq.c, which RFC 6716 section 4.3.4.3 names without stating the procedure.

// The four spreading amounts, in increasing order of rotation.
const (
	SpreadNone = iota
	SpreadLight
	SpreadNormal
	SpreadAggressive
)

// spreadICDF is the distribution of the spreading decision, from celt/celt.c.
var spreadICDF = []byte{25, 23, 2, 0}

// spreadFactor sets the rotation angle for each amount above none, from celt/vq.c. A larger factor
// makes a smaller angle, so light spreading rotates furthest.
var spreadFactor = [3]int{15, 10, 5}

// DecodeSpread reads the frame's spreading amount. RFC 6716 section 4.3.4.3.
//
// The symbol is skipped when fewer than four bits remain, in which case the amount is normal. That
// keeps a frame that has run out of room in step with the encoder instead of desynchronising.
func DecodeSpread(d *rangecoder.Decoder, totalBits int) int {
	if d.Tell()+4 > totalBits {
		return SpreadNormal
	}
	return d.DecodeICDF(spreadICDF, 5)
}

// Rotate applies the spreading rotation to a band of n coefficients split into the given number of
// interleaved blocks, carrying k pulses.
//
// A forward rotation precedes quantisation and an inverse one follows dequantisation, so the decoder
// wants the inverse. Both directions are here because the pair is what makes the transform testable:
// nothing outside the two of them can say whether the angle is right.
func Rotate(x []float32, blocks, k, spread int, forward bool) {
	n := len(x)
	// Densely allocated bands are already spread out, and rotating them would only smear detail. The
	// reference draws the line at a pulse for every other coefficient.
	if spread == SpreadNone || 2*k >= n || blocks <= 0 || n == 0 {
		return
	}

	factor := spreadFactor[spread-1]
	gain := float64(n) / float64(n+factor*k)
	theta := gain * gain / 2

	// The angle is a quarter turn scaled by theta, which is how celt_cos_norm reads its argument.
	c := float32(cosNorm(theta))
	s := float32(cosNorm(1 - theta))

	// A wide enough band gets a second pass at a longer stride, which spreads energy across the block
	// rather than only between neighbours.
	stride2 := 0
	if n >= 8*blocks {
		stride2 = 1
		// The smallest stride2 with (stride2+0.5)^2 >= n/blocks, by increment rather than a square root.
		for (stride2*stride2+stride2)*blocks+(blocks>>2) < n {
			stride2++
		}
	}

	blockLen := n / blocks
	for i := range blocks {
		block := x[i*blockLen : (i+1)*blockLen]
		if forward {
			givens(block, 1, c, -s)
			if stride2 != 0 {
				givens(block, stride2, s, -c)
			}
		} else {
			if stride2 != 0 {
				givens(block, stride2, s, c)
			}
			givens(block, 1, c, s)
		}
	}
}

// givens rotates every pair of coefficients a stride apart, sweeping up and then back down.
//
// The two sweeps are what carry energy across the whole block: one pass alone would only move it in
// one direction, since each rotation feeds the next.
func givens(x []float32, stride int, c, s float32) {
	n := len(x)
	for i := 0; i < n-stride; i++ {
		x1, x2 := x[i], x[i+stride]
		x[i+stride] = c*x2 + s*x1
		x[i] = c*x1 - s*x2
	}
	for i := n - 2*stride - 1; i >= 0; i-- {
		x1, x2 := x[i], x[i+stride]
		x[i+stride] = c*x2 + s*x1
		x[i] = c*x1 - s*x2
	}
}

// CollapseMask records which blocks of a band received at least one pulse.
//
// A block left empty by the quantiser is a collapsed one, and the anti-collapse pass of RFC 6716
// section 4.3.5 refills it with noise. The mask reads the integer pulse vector rather than the shape,
// so the rotation cannot obscure it; rotation works inside a block in any case.
func CollapseMask(pulses []int, blocks int) uint32 {
	if blocks <= 1 {
		return 1
	}
	blockLen := len(pulses) / blocks
	var mask uint32
	for i := range blocks {
		for _, v := range pulses[i*blockLen : (i+1)*blockLen] {
			if v != 0 {
				mask |= 1 << uint(i)
				break
			}
		}
	}
	return mask
}

// cosNorm is the reference's celt_cos_norm: a cosine whose argument is in quarter turns.
func cosNorm(x float64) float64 { return math.Cos(math.Pi / 2 * x) }
