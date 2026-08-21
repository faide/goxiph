package celt

import (
	"math"
	"math/bits"
)

// The band decoder reshapes a band before quantising it, trading frequency resolution for time
// resolution where the signal calls for it, and it splits a band that would need more bits than one
// codebook can address. The pieces here are the reshaping and the arithmetic the split depends on.
//
// Adapted from celt/bands.c and celt/rate.h, which RFC 6716 section 4.3.4.4 names without stating
// the procedure.

// haar1 applies one stage of a Haar transform over pairs of coefficients a stride apart.
//
// Running it forward halves the number of blocks and doubles their length, which converts frequency
// resolution into time resolution; the transform is its own inverse, so the decoder undoes a stage
// by applying another.
func haar1(x []float32, n0, stride int) {
	const invSqrt2 = float32(math.Sqrt2 / 2)
	n0 >>= 1
	for i := range stride {
		for j := range n0 {
			a := invSqrt2 * x[stride*2*j+i]
			b := invSqrt2 * x[stride*(2*j+1)+i]
			x[stride*2*j+i] = a + b
			x[stride*(2*j+1)+i] = a - b
		}
	}
}

// orderyTable gives the block order for the Hadamard reorganisation, for strides 2, 4, 8 and 16.
//
// The rows are laid end to end and indexed by stride-2, which is how the reference reaches them.
var orderyTable = []int{
	1, 0,
	3, 0, 2, 1,
	7, 0, 4, 3, 6, 1, 5, 2,
	15, 0, 8, 7, 12, 3, 11, 4, 14, 1, 9, 6, 13, 2, 10, 5,
}

// deinterleaveHadamard gathers interleaved short blocks into consecutive runs, so a band that was
// stored in frequency order is laid out in time order.
//
// With hadamard set the blocks are also permuted, which groups the ones a Haar stage has correlated.
func deinterleaveHadamard(x []float32, n0, stride int, hadamard bool) {
	tmp := make([]float32, n0*stride)
	for i := range stride {
		dst := i
		if hadamard {
			dst = orderyTable[stride-2+i]
		}
		for j := range n0 {
			tmp[dst*n0+j] = x[j*stride+i]
		}
	}
	copy(x, tmp)
}

// interleaveHadamard is the inverse of deinterleaveHadamard.
func interleaveHadamard(x []float32, n0, stride int, hadamard bool) {
	tmp := make([]float32, n0*stride)
	for i := range stride {
		src := i
		if hadamard {
			src = orderyTable[stride-2+i]
		}
		for j := range n0 {
			tmp[j*stride+i] = x[src*n0+j]
		}
	}
	copy(x, tmp)
}

// bitInterleaveTable and bitDeinterleaveTable carry a collapse mask through the reshaping, so a
// block that received no pulses is still identifiable after its neighbours have been mixed into it.
var (
	bitInterleaveTable   = [16]byte{0, 1, 1, 1, 2, 3, 3, 3, 2, 3, 3, 3, 2, 3, 3, 3}
	bitDeinterleaveTable = [16]byte{
		0x00, 0x03, 0x0C, 0x0F, 0x30, 0x33, 0x3C, 0x3F,
		0xC0, 0xC3, 0xCC, 0xCF, 0xF0, 0xF3, 0xFC, 0xFF,
	}
)

// fracMul16 is the reference's FRAC_MUL16: a rounded 16-bit multiply in Q15.
func fracMul16(a, b int32) int32 {
	return (16384 + int32(int16(a))*int32(int16(b))) >> 15
}

// bitexactCos approximates a quarter-turn cosine in Q14 in, Q15 out.
//
// It is a polynomial in fixed point rather than a call to cos, because the result feeds the split's
// bit allocation. A platform that rounded differently would allocate differently and desynchronise,
// so the approximation is part of the format rather than an implementation choice.
func bitexactCos(x int32) int32 {
	tmp := (4096 + x*x) >> 13
	x2 := int32(int16(tmp))
	x2 = (32767 - x2) + fracMul16(x2, -7651+fracMul16(x2, 8277+fracMul16(-626, x2)))
	return 1 + x2
}

// bitexactLog2Tan returns the base-two log of the tangent given by a sine and cosine pair, in Q11.
//
// This is what turns a split angle into a bit split between the two halves: the log of the ratio is
// the difference in the bits each half deserves.
func bitexactLog2Tan(isin, icos int32) int32 {
	ls := int32(bits.Len32(uint32(isin)))
	lc := int32(bits.Len32(uint32(icos)))
	isin <<= uint(15 - ls)
	icos <<= uint(15 - lc)
	return (ls-lc)*(1<<11) +
		fracMul16(isin, fracMul16(isin, -2597)+7932) -
		fracMul16(icos, fracMul16(icos, -2597)+7932)
}

// The two offsets subtracted from half the pulse cap when sizing the split angle, from celt/rate.h.
const (
	qthetaOffset         = 4
	qthetaOffsetTwoPhase = 16
)

// thetaOffset returns the offset computeQn applies to the budget.
//
// A two-sample stereo band codes its side with a single sign bit, so it needs a coarser angle than
// its width alone suggests and takes the larger offset.
func thetaOffset(pulseCap, n int, stereo bool) int {
	if stereo && n == 2 {
		return (pulseCap >> 1) - qthetaOffsetTwoPhase
	}
	return (pulseCap >> 1) - qthetaOffset
}

// exp2Table8 holds two to the power of k/8 in Q14, for k below eight.
var exp2Table8 = [8]int32{16384, 17866, 19483, 21247, 23170, 25267, 27554, 30048}

// computeQn returns how many steps the split angle is quantised into.
//
// A wider band and a larger budget buy a finer angle. The upper bound on qb is what guarantees that
// a stereo split at the extreme angle still leaves room for one pulse in the side, which would
// otherwise collapse with nothing to fold into it.
func computeQn(n, b, offset, pulseCap int, stereo bool) int {
	n2 := 2*n - 1
	if stereo && n == 2 {
		n2--
	}

	qb := min(b-pulseCap-(4<<BitRes), (b+n2*offset)/n2)
	qb = min(8<<BitRes, qb)

	if qb < (1 << BitRes >> 1) {
		return 1
	}
	qn := int(exp2Table8[qb&0x7] >> uint(14-(qb>>BitRes)))
	return (qn + 1) >> 1 << 1
}

// lcgRand is the noise generator used to fill a band that received no pulses.
//
// The decoder must produce the same noise the encoder assumed, so the generator is specified rather
// than chosen: it is a plain linear congruential step seeded from the frame.
func lcgRand(seed uint32) uint32 {
	return 1664525*seed + 1013904223
}

// renormalise scales a vector back to the given gain, which the noise and folding fills need after
// they have put arbitrary values into a band.
func renormalise(x []float32, gain float32) {
	var e float64
	for _, v := range x {
		e += float64(v) * float64(v)
	}
	if e == 0 {
		return
	}
	g := float32(float64(gain) / math.Sqrt(e))
	for i := range x {
		x[i] *= g
	}
}
