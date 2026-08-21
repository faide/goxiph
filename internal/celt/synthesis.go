package celt

// After the transform a frame still needs two filters. The post-filter puts back the pitch
// structure that coding a periodic signal at low rates flattens out, and the de-emphasis undoes the
// tilt the encoder applied to spread quantisation noise more evenly across frequency.
//
// Adapted from celt/celt.c.

// The output history a frame can reach back into, and the bounds on the pitch period.
const (
	decodeBufferSize = 2048
	combMaxPeriod    = 1024
	combMinPeriod    = 15
)

// preemphCoef is the emphasis filter, from celt/static_modes_float.h.
var preemphCoef = [4]float32{0.85000610, 0, 1, 1}

// sigScale is the amplitude the coded energies are expressed against. Dividing by it puts the output
// in the range a sample is normally written in.
const sigScale = 32768

// combGains are the three-tap filter shapes, from celt/celt.c. A higher tap set is more sharply
// peaked, so it reinforces the pitch harder and colours the signal more.
var combGains = [3][3]float32{
	{0.3066406250, 0.2170410156, 0.1296386719},
	{0.4638671875, 0.2680664062, 0},
	{0.7998046875, 0.1000976562, 0},
}

// combFilter reinforces the pitch period across n samples starting at off.
//
// It filters in place, so a tap reaching back less than n samples reads output the filter has
// already produced. That recursion is what lets a three-tap filter build a comb sharp enough to
// matter at these rates.
//
// The first overlap samples cross-fade from the previous frame's period and gain to this frame's, so
// a pitch that moves does not step.
func combFilter(buf []float32, off, t0, t1, n int, g0, g1 float32,
	tapset0, tapset1 int, window []float32, overlap int,
) {
	combFilterTo(buf, off, buf, off, t0, t1, n, g0, g1, tapset0, tapset1, window, overlap)
}

// combFilterTo is the same filter reading one buffer and writing another.
//
// Reading and writing the same buffer makes the filter recursive, which is what sharpens the comb;
// concealment wants the plain form for one of its two passes, where the input is the previous
// frame's tail and the output is a working copy of it.
func combFilterTo(dst []float32, dstOff int, src []float32, srcOff, t0, t1, n int, g0, g1 float32,
	tapset0, tapset1 int, window []float32, overlap int,
) {
	if g0 == 0 && g1 == 0 {
		if &dst[0] != &src[0] || dstOff != srcOff {
			copy(dst[dstOff:dstOff+n], src[srcOff:srcOff+n])
		}
		return
	}

	g00 := g0 * combGains[tapset0][0]
	g01 := g0 * combGains[tapset0][1]
	g02 := g0 * combGains[tapset0][2]
	g10 := g1 * combGains[tapset1][0]
	g11 := g1 * combGains[tapset1][1]
	g12 := g1 * combGains[tapset1][2]

	tap := func(i, t int, a, b, c float32) float32 {
		p := srcOff + i - t
		if p-2 < 0 || p+2 >= len(src) {
			return 0
		}
		return a*src[p] + b*(src[p-1]+src[p+1]) + c*(src[p-2]+src[p+2])
	}

	limit := min(overlap, n)
	for i := range limit {
		// The cross-fade is on the squared window, so the two filters sum to one in power.
		f := window[i] * window[i]
		dst[dstOff+i] = src[srcOff+i] +
			(1-f)*tap(i, t0, g00, g01, g02) + f*tap(i, t1, g10, g11, g12)
	}
	for i := limit; i < n; i++ {
		dst[dstOff+i] = src[srcOff+i] + tap(i, t1, g10, g11, g12)
	}
}

// deemphasis undoes the encoder's pre-emphasis and writes the frame's samples.
//
// The filter carries state between frames, so its memory is per channel and per stream.
func deemphasis(dst, src []float32, srcOff, n int, mem *float32) {
	m := *mem
	for j := range n {
		x := src[srcOff+j]
		sum := x + m
		m = preemphCoef[0]*sum - preemphCoef[1]*x
		dst[j] = preemphCoef[3] * sum / sigScale
	}
	*mem = m
}
