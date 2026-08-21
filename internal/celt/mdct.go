package celt

import "math"

// The inverse transform turns a frame's spectrum back into samples. CELT folds the windowing and the
// time-domain aliasing into the transform itself rather than applying them afterwards, so the output
// is already the overlapping tail the next frame adds to.
//
// The window is short: it tapers over 120 samples at each end and is flat between. That keeps a
// frame's influence local, which is what lets the codec run at low latency.
//
// Adapted from celt/mdct.c and celt/modes.c.

// smallestFactor returns the smallest prime factor of n above one.
func smallestFactor(n int) int {
	for p := 2; p*p <= n; p++ {
		if n%p == 0 {
			return p
		}
	}
	return n
}

// fftPlan holds the twiddle factors for one transform size.
//
// CELT's sizes are sixty times a power of two, so they are not powers of two themselves and a
// radix-two transform cannot do them. The factors here are whatever n has.
type fftPlan struct {
	n  int
	tw []complex128 // tw[k] = exp(2i*pi*k/n)
	// scratch is reused by the combine step, whose width is the current factor.
	scratch []complex128
}

func newFFTPlan(n int) *fftPlan {
	p := &fftPlan{n: n, tw: make([]complex128, n)}
	for k := range n {
		a := 2 * math.Pi * float64(k) / float64(n)
		p.tw[k] = complex(math.Cos(a), math.Sin(a))
	}

	// The widest factor bounds the combine step's working set.
	widest := 1
	for m, r := n, 0; m > 1; m /= r {
		r = smallestFactor(m)
		widest = max(widest, r)
	}
	p.scratch = make([]complex128, widest)
	return p
}

// inverse computes the unscaled inverse discrete Fourier transform of src into dst.
//
// Unscaled is what the transform above it expects: the factor of one over n is absorbed into the
// windowing, so applying it here would halve the output twice.
func (p *fftPlan) inverse(dst, src []complex128) {
	p.run(dst, src, p.n, 1, 1)
}

// run is a decimation-in-time Cooley-Tukey step.
//
// stride walks the input, and twStride selects every twStride'th twiddle so one table serves every
// depth: a sub-transform of size m reads the table n/m entries apart.
func (p *fftPlan) run(dst, src []complex128, n, stride, twStride int) {
	if n == 1 {
		dst[0] = src[0]
		return
	}

	r := smallestFactor(n)
	m := n / r
	for j := range r {
		p.run(dst[j*m:(j+1)*m], src[j*stride:], m, stride*r, twStride*r)
	}

	buf := p.scratch[:r]
	for k := range m {
		for j := range r {
			buf[j] = dst[j*m+k] * p.tw[(j*k*twStride)%p.n]
		}
		for q := range r {
			var sum complex128
			for j := range r {
				sum += buf[j] * p.tw[(j*q*m*twStride)%p.n]
			}
			dst[q*m+k] = sum
		}
	}
}

// Window returns the overlap window, which tapers a frame's ends so consecutive frames sum to one.
//
// From celt/modes.c: a sine of a squared sine, which satisfies the power-complementary condition the
// transform's aliasing cancellation needs.
func Window(overlap int) []float32 {
	w := make([]float32, overlap)
	for i := range overlap {
		s := math.Sin(math.Pi / 2 * (float64(i) + 0.5) / float64(overlap))
		w[i] = float32(math.Sin(math.Pi / 2 * s * s))
	}
	return w
}

// Overlap is the window length, and the number of samples a frame shares with the next.
const Overlap = 120

// MDCT performs CELT's inverse transform at every frame size the format uses.
type MDCT struct {
	n      int       // the largest transform size, in samples
	trig   []float64 // trig[k] = cos(2*pi*k/n), for k up to n/4
	window []float32
	plans  map[int]*fftPlan
}

// NewMDCT returns a transform for frames of up to maxSamples.
func NewMDCT(maxSamples int) *MDCT {
	n := 2 * maxSamples
	t := &MDCT{n: n, window: Window(Overlap), plans: map[int]*fftPlan{}}

	t.trig = make([]float64, n/4+1)
	for i := range t.trig {
		t.trig[i] = math.Cos(2 * math.Pi * float64(i) / float64(n))
	}
	// The transform runs at every halving down to the shortest block, and no further.
	for shift := 0; (n >> uint(shift)) >= 2*ShortMdctSize; shift++ {
		size := (n >> uint(shift)) / 4
		t.plans[size] = newFFTPlan(size)
	}
	return t
}

// Window returns the overlap window this transform applies.
func (t *MDCT) Window() []float32 { return t.window }

// Backward runs one inverse transform, adding its windowed ends into out.
//
// in holds the spectrum, read every stride'th entry so that interleaved short blocks can be taken in
// turn. shift selects the size: the transform runs at n>>shift samples. off is where in out the
// block begins, and may be negative for the first block of a frame, whose leading taper falls into
// the tail the previous frame already wrote.
//
// The ends are added rather than assigned on the leading side and assigned on the trailing side,
// which is what makes consecutive blocks overlap correctly without a separate accumulation pass.
func (t *MDCT) Backward(in []float32, stride int, out []float32, off, shift int) {
	n := t.n >> uint(shift)
	n2, n4 := n>>1, n>>2
	overlap := Overlap

	// The twiddle applied after each rotation is a rotation by an eighth of a bin. It is written as a
	// small correction because at these sizes the cosine of it is indistinguishable from one.
	sine := 2 * math.Pi * 0.125 / float64(n)

	pre := make([]complex128, n4)
	post := make([]complex128, n4)
	// The aliasing step reads this as a flat sequence rather than as complex pairs, taking the two
	// halves from the middle outwards, so it is kept flat rather than reinterpreted.
	f2 := make([]float64, n2)
	trigAt := func(i int) float64 { return t.trig[i<<uint(shift)] }

	// Pre-rotate the spectrum into a half-length complex sequence.
	for i := range n4 {
		x1 := float64(in[2*i*stride])
		x2 := float64(in[stride*(n2-1-2*i)])
		yr := -x2*trigAt(i) + x1*trigAt(n4-i)
		yi := -x2*trigAt(n4-i) - x1*trigAt(i)
		pre[i] = complex(yr-yi*sine, yi+yr*sine)
	}

	t.plans[n4].inverse(post, pre)

	// Post-rotate, then de-shuffle. The real parts run forwards and the imaginary parts backwards,
	// which is what puts the two mirrored halves next to each other.
	for i := range n4 {
		re, im := real(post[i]), imag(post[i])
		yr := re*trigAt(i) - im*trigAt(n4-i)
		yi := im*trigAt(i) + re*trigAt(n4-i)
		post[i] = complex(yr-yi*sine, yi+yr*sine)
	}
	for i := range n4 {
		f2[2*i] = -real(post[i])
		f2[2*i+1] = imag(post[n4-1-i])
	}

	// Mirror both halves, windowing only where the taper reaches. Everything between is copied.
	base := off - ((n2 - overlap) >> 1)
	flat := n4 - overlap/2

	fp1, xp1 := n4-1, base+n2-1
	for range flat {
		out[xp1] = float32(f2[fp1])
		xp1--
		fp1--
	}
	yp1 := base + flat
	for j := range overlap / 2 {
		v := f2[fp1]
		fp1--
		out[yp1] += float32(-float64(t.window[j]) * v)
		yp1++
		out[xp1] += float32(float64(t.window[overlap-1-j]) * v)
		xp1--
	}

	fp2, xp2 := n4, base+n2
	for range flat {
		out[xp2] = float32(f2[fp2])
		xp2++
		fp2++
	}
	yp2 := base + n - 1 - flat
	for j := range overlap / 2 {
		v := f2[fp2]
		fp2++
		out[yp2] = float32(float64(t.window[j]) * v)
		yp2--
		out[xp2] = float32(float64(t.window[overlap-1-j]) * v)
		xp2++
	}
}

// InverseFrame runs the inverse transform for a whole frame, short blocks and all.
//
// out must hold the frame plus one overlap, and its leading overlap must be zero: the first block's
// taper is added into it.
func (t *MDCT) InverseFrame(spectrum, out []float32, lm int, shortBlocks bool, maxLM int) {
	n := ShortMdctSize << uint(lm)

	blocks, blockLen, shift := 1, n, maxLM-lm
	if shortBlocks {
		blocks, blockLen, shift = 1<<uint(lm), ShortMdctSize, maxLM
	}

	clear(out[:Overlap])
	for b := range blocks {
		t.Backward(spectrum[b:], blocks, out, blockLen*b, shift)
	}
}
