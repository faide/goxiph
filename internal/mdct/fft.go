package mdct

import (
	"math"
	"math/bits"
)

// fftPlan computes an in-place radix-2 complex DFT of a fixed power-of-two size.
//
// float64 throughout: the transform sums thousands of terms and the decoder is checked against a
// reference to within one 16-bit LSB, which leaves no headroom for float32 accumulation error.
type fftPlan struct {
	n   int
	rev []int32   // bit-reversal permutation
	cos []float64 // twiddles for each stage, concatenated
	sin []float64
}

func newFFTPlan(n int) *fftPlan {
	p := &fftPlan{n: n, rev: make([]int32, n)}

	shift := bits.TrailingZeros(uint(n))
	for i := range n {
		p.rev[i] = int32(bits.Reverse(uint(i)) >> (bits.UintSize - shift))
	}

	// Half a turn of twiddles is enough: stage m uses stride n/m.
	p.cos = make([]float64, n/2)
	p.sin = make([]float64, n/2)
	for i := range n / 2 {
		angle := -2 * math.Pi * float64(i) / float64(n)
		p.cos[i] = math.Cos(angle)
		p.sin[i] = math.Sin(angle)
	}
	return p
}

// transform replaces re and im with their forward DFT, using the exp(-2i*pi*k*n/N) convention.
func (p *fftPlan) transform(re, im []float64) {
	n := p.n

	for i := range n {
		j := int(p.rev[i])
		if j > i {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}

	for size := 2; size <= n; size <<= 1 {
		half := size / 2
		stride := n / size
		for start := 0; start < n; start += size {
			for k := range half {
				wr := p.cos[k*stride]
				wi := p.sin[k*stride]
				a := start + k
				b := a + half

				tr := re[b]*wr - im[b]*wi
				ti := re[b]*wi + im[b]*wr

				re[b] = re[a] - tr
				im[b] = im[a] - ti
				re[a] += tr
				im[a] += ti
			}
		}
	}
}
