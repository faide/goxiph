package mdct

import (
	"math"
	"math/rand/v2"
	"testing"
)

// sineWindow is the Vorbis slope function, which satisfies the Princen-Bradley condition and so
// permits perfect reconstruction under overlap-add.
func sineWindow(size int) []float32 {
	w := make([]float32, size)
	half := float64(size / 2)
	for i := range w {
		x := (float64(i) + 0.5) / half * math.Pi / 2
		w[i] = float32(math.Sin(math.Pi / 2 * math.Sin(x) * math.Sin(x)))
	}
	return w
}

// TestPerfectReconstruction is the test that pins the normalisation.
//
// One MDCT followed by one inverse cannot recover its input: half the information is folded away.
// Overlapping consecutive frames by half and adding them does recover it, and only for the correct
// scale factor. A wrong constant anywhere in the pair shows up here as a uniform amplitude error.
func TestPerfectReconstruction(t *testing.T) {
	for _, n := range []int{8, 32, 128, 256} {
		size := 2 * n
		tr, err := New(n)
		if err != nil {
			t.Fatalf("New(%d): %v", n, err)
		}
		w := sineWindow(size)

		// A signal long enough for several overlapping frames.
		rng := rand.New(rand.NewPCG(7, 11))
		total := size * 6
		signal := make([]float32, total)
		for i := range signal {
			signal[i] = rng.Float32()*2 - 1
		}

		out := make([]float32, total)
		spectrum := make([]float32, n)
		windowed := make([]float32, size)
		frame := make([]float32, size)

		// Frames advance by n, so each sample is covered by exactly two frames.
		for start := 0; start+size <= total; start += n {
			for j := range size {
				windowed[j] = signal[start+j] * w[j]
			}
			if err := tr.Forward(windowed, spectrum); err != nil {
				t.Fatalf("Forward: %v", err)
			}
			if err := tr.Inverse(spectrum, frame); err != nil {
				t.Fatalf("Inverse: %v", err)
			}
			for j := range size {
				out[start+j] += frame[j] * w[j]
			}
		}

		// Only the interior is covered by two frames; the edges are primed and are not compared.
		var maxErr float64
		for i := size; i < total-size; i++ {
			e := math.Abs(float64(out[i] - signal[i]))
			maxErr = math.Max(maxErr, e)
		}
		if maxErr > 1e-4 {
			t.Errorf("n=%d: reconstruction error %g, want under 1e-4", n, maxErr)
		}
	}
}

// TestWindowSatisfiesPrincenBradley checks the property perfect reconstruction depends on.
func TestWindowSatisfiesPrincenBradley(t *testing.T) {
	for _, size := range []int{16, 64, 256, 2048} {
		w := sineWindow(size)
		half := size / 2
		for j := range half {
			sum := float64(w[j])*float64(w[j]) + float64(w[j+half])*float64(w[j+half])
			if math.Abs(sum-1) > 1e-6 {
				t.Fatalf("size %d, index %d: w^2 + w^2 = %g, want 1", size, j, sum)
			}
		}
	}
}

// TestInverseOfImpulseIsCosine checks the transform against its closed form: a single spectral
// coefficient must produce exactly the corresponding cosine basis function.
func TestInverseOfImpulseIsCosine(t *testing.T) {
	const n = 16
	tr, err := New(n)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, k := range []int{0, 1, 5, n - 1} {
		spectrum := make([]float32, n)
		spectrum[k] = 1
		out := make([]float32, 2*n)
		if err := tr.Inverse(spectrum, out); err != nil {
			t.Fatalf("Inverse: %v", err)
		}

		for j := range out {
			want := math.Cos(math.Pi / float64(n) * (float64(j) + 0.5 + float64(n)/2) * (float64(k) + 0.5))
			if math.Abs(float64(out[j])-want) > 1e-5 {
				t.Fatalf("k=%d j=%d: got %g, want %g", k, j, out[j], want)
			}
		}
	}
}

// TestForwardIsLinear checks a property that catches indexing mistakes the reconstruction test can
// mask.
func TestForwardIsLinear(t *testing.T) {
	const n = 32
	tr, _ := New(n)
	rng := rand.New(rand.NewPCG(3, 5))

	a := make([]float32, 2*n)
	b := make([]float32, 2*n)
	sum := make([]float32, 2*n)
	for i := range a {
		a[i] = rng.Float32() - 0.5
		b[i] = rng.Float32() - 0.5
		sum[i] = a[i] + b[i]
	}

	sa := make([]float32, n)
	sb := make([]float32, n)
	ssum := make([]float32, n)
	_ = tr.Forward(a, sa)
	_ = tr.Forward(b, sb)
	_ = tr.Forward(sum, ssum)

	for k := range n {
		if math.Abs(float64(sa[k]+sb[k]-ssum[k])) > 1e-3 {
			t.Fatalf("k=%d: %g + %g != %g", k, sa[k], sb[k], ssum[k])
		}
	}
}

// TestTimeDomainAliasing documents why a single inverse is not enough: the first half of the output
// is the reflected negative of itself about its centre, and the second half likewise.
func TestTimeDomainAliasing(t *testing.T) {
	const n = 16
	tr, _ := New(n)
	rng := rand.New(rand.NewPCG(9, 13))

	in := make([]float32, 2*n)
	for i := range in {
		in[i] = rng.Float32() - 0.5
	}
	spectrum := make([]float32, n)
	out := make([]float32, 2*n)
	_ = tr.Forward(in, spectrum)
	_ = tr.Inverse(spectrum, out)

	// Aliasing means out != in; if they matched, the transform would not be folding.
	same := true
	for i := range in {
		if math.Abs(float64(in[i]-out[i])) > 1e-4 {
			same = false
			break
		}
	}
	if same {
		t.Error("a single forward and inverse recovered the input, so no aliasing is happening")
	}
}

func TestNewRejectsBadSizes(t *testing.T) {
	for _, n := range []int{0, -4, 3, 15} {
		if _, err := New(n); err == nil {
			t.Errorf("New(%d) accepted an invalid size", n)
		}
	}
}

func TestWrongBufferSizes(t *testing.T) {
	tr, _ := New(8)
	if err := tr.Forward(make([]float32, 8), make([]float32, 8)); err == nil {
		t.Error("Forward accepted a wrong input length")
	}
	if err := tr.Inverse(make([]float32, 8), make([]float32, 8)); err == nil {
		t.Error("Inverse accepted a wrong output length")
	}
}

func BenchmarkInverse(b *testing.B) {
	for _, n := range []int{128, 1024} {
		tr, _ := New(n)
		spectrum := make([]float32, n)
		for i := range spectrum {
			spectrum[i] = float32(i%17) / 17
		}
		out := make([]float32, 2*n)
		b.Run(string(rune('0'+n/1000))+"k", func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				_ = tr.Inverse(spectrum, out)
			}
		})
	}
}

// TestFastMatchesDirect is the gate on the FFT path. The direct evaluation is the definition, so
// any disagreement is a bug in the fast route rather than a question of tolerance.
func TestFastMatchesDirect(t *testing.T) {
	rng := rand.New(rand.NewPCG(17, 23))

	for _, n := range []int{2, 4, 8, 16, 64, 128, 512, 1024, 4096} {
		tr, err := New(n)
		if err != nil {
			t.Fatalf("New(%d): %v", n, err)
		}
		if tr.plan == nil {
			t.Fatalf("n=%d is a power of two but got no fast path", n)
		}

		spectrum := make([]float32, n)
		for i := range spectrum {
			spectrum[i] = rng.Float32()*2 - 1
		}

		fast := make([]float32, 2*n)
		direct := make([]float32, 2*n)
		tr.inverseFast(spectrum, fast)
		tr.inverseDirect(spectrum, direct)

		var worst float64
		for j := range fast {
			d := math.Abs(float64(fast[j] - direct[j]))
			worst = math.Max(worst, d)
		}
		// The direct path accumulates n terms in float32, so it is the less accurate of the two;
		// the bound scales with that rather than with the FFT.
		limit := 1e-4 * float64(n)
		if worst > limit {
			t.Errorf("n=%d: fast and direct differ by %g, limit %g", n, worst, limit)
		}
	}
}

// TestFastImpulseIsExact checks the fast path against the closed form rather than against the other
// implementation, so a shared misconception in both would still be caught.
func TestFastImpulseIsExact(t *testing.T) {
	const n = 64
	tr, _ := New(n)

	for _, k := range []int{0, 1, 7, 32, n - 1} {
		spectrum := make([]float32, n)
		spectrum[k] = 1
		out := make([]float32, 2*n)
		tr.inverseFast(spectrum, out)

		for j := range out {
			want := math.Cos(math.Pi / float64(n) * (float64(j) + 0.5 + float64(n)/2) * (float64(k) + 0.5))
			if math.Abs(float64(out[j])-want) > 1e-5 {
				t.Fatalf("k=%d j=%d: got %g, want %g", k, j, out[j], want)
			}
		}
	}
}

func TestFFTAgainstDirectDFT(t *testing.T) {
	const n = 32
	p := newFFTPlan(n)
	rng := rand.New(rand.NewPCG(5, 9))

	re := make([]float64, n)
	im := make([]float64, n)
	origRe := make([]float64, n)
	origIm := make([]float64, n)
	for i := range n {
		re[i] = rng.Float64() - 0.5
		im[i] = rng.Float64() - 0.5
		origRe[i], origIm[i] = re[i], im[i]
	}
	p.transform(re, im)

	for k := range n {
		var wr, wi float64
		for j := range n {
			a := -2 * math.Pi * float64(k) * float64(j) / float64(n)
			c, s := math.Cos(a), math.Sin(a)
			wr += origRe[j]*c - origIm[j]*s
			wi += origRe[j]*s + origIm[j]*c
		}
		if math.Abs(re[k]-wr) > 1e-9 || math.Abs(im[k]-wi) > 1e-9 {
			t.Fatalf("bin %d: got (%g,%g), want (%g,%g)", k, re[k], im[k], wr, wi)
		}
	}
}
