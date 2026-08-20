// Package mdct implements the modified discrete cosine transform used by transform codecs.
//
// The transform maps 2N time samples onto N spectral coefficients and back. It is lossy in one
// direction alone: a single inverse cannot recover its input, but overlapping consecutive frames by
// half and adding them does, provided the analysis window satisfies the Princen-Bradley condition
// w[j]^2 + w[j+N]^2 = 1. That property is what TestPerfectReconstruction checks.
package mdct

import (
	"fmt"
	"math"
)

// Transform performs forward and inverse MDCT for one spectrum size.
//
// A Transform is not safe for concurrent use; give each stream its own.
type Transform struct {
	n   int // spectral coefficients; the time domain holds 2n samples
	cos []float32
}

// New returns a Transform producing n spectral coefficients from 2n samples.
func New(n int) (*Transform, error) {
	if n <= 0 || n%2 != 0 {
		return nil, fmt.Errorf("mdct: spectrum size %d must be positive and even", n)
	}

	// The cosine argument is always m*pi/(4n) for an integer m, and cos is periodic in m with
	// period 8n. Tabulating one period turns the inner loop into a multiply-add and an index,
	// which matters because it runs n times per output sample.
	t := &Transform{n: n, cos: make([]float32, 8*n)}
	for m := range t.cos {
		t.cos[m] = float32(math.Cos(float64(m) * math.Pi / float64(4*n)))
	}
	return t, nil
}

// N is the number of spectral coefficients.
func (t *Transform) N() int { return t.n }

// coefficient returns cos(pi/n * (j + 1/2 + n/2) * (k + 1/2)) from the table.
func (t *Transform) coefficient(j, k int) float32 {
	m := (2*j + 1 + t.n) * (2*k + 1)
	return t.cos[m&(8*t.n-1)]
}

// Forward transforms 2n time samples into n spectral coefficients.
//
// The caller applies the window before calling; the transform itself does not window.
//
// The 2/n normalisation of the transform pair lives here rather than in Inverse because Vorbis
// encoders fold it into the spectrum they transmit, leaving decoders an unscaled inverse. Splitting
// it the other way reconstructs just as well in isolation and decodes every real stream 2/n too
// quiet.
func (t *Transform) Forward(in, spectrum []float32) error {
	if len(in) != 2*t.n || len(spectrum) != t.n {
		return fmt.Errorf("mdct: forward wants %d samples and %d coefficients, got %d and %d",
			2*t.n, t.n, len(in), len(spectrum))
	}
	scale := 2 / float32(t.n)
	for k := range spectrum {
		var sum float32
		for j, x := range in {
			sum += x * t.coefficient(j, k)
		}
		spectrum[k] = sum * scale
	}
	return nil
}

// Inverse transforms n spectral coefficients into 2n time samples, unscaled.
//
// The output still needs windowing and overlap-add before it is audio. See Forward for where the
// normalisation lives and why.
func (t *Transform) Inverse(spectrum, out []float32) error {
	if len(spectrum) != t.n || len(out) != 2*t.n {
		return fmt.Errorf("mdct: inverse wants %d coefficients and %d samples, got %d and %d",
			t.n, 2*t.n, len(spectrum), len(out))
	}
	for j := range out {
		var sum float32
		for k, x := range spectrum {
			sum += x * t.coefficient(j, k)
		}
		out[j] = sum
	}
	return nil
}
