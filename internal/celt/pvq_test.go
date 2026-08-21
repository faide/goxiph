package celt

import (
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/faide/goxiph/internal/rangecoder"
)

// countVectorsByEnumeration counts vectors of n integers summing to k in absolute value, by walking
// every one of them.
//
// Exponentially slow, and that is the point: it shares nothing with the recurrence, so agreement
// between the two is evidence rather than a tautology.
func countVectorsByEnumeration(n, k int) int {
	if n == 0 {
		if k == 0 {
			return 1
		}
		return 0
	}
	total := 0
	for v := -k; v <= k; v++ {
		mag := v
		if mag < 0 {
			mag = -mag
		}
		total += countVectorsByEnumeration(n-1, k-mag)
	}
	return total
}

// TestCombinationCountAgainstEnumeration checks the recurrence against brute force.
func TestCombinationCountAgainstEnumeration(t *testing.T) {
	for n := range 7 {
		for k := range 8 {
			want := countVectorsByEnumeration(n, k)
			if got := int(CombinationCount(n, k)); got != want {
				t.Errorf("V(%d, %d) = %d, want %d", n, k, got, want)
			}
		}
	}
}

// TestCombinationCountKnownValues pins a few entries that can be reasoned about directly.
func TestCombinationCountKnownValues(t *testing.T) {
	cases := []struct {
		n, k int
		want uint32
	}{
		{0, 0, 1}, // the empty vector
		{5, 0, 1}, // all zeros, however many coordinates
		{0, 3, 0}, // no coordinates cannot carry pulses
		{1, 1, 2}, // plus one and minus one
		{1, 7, 2}, // still only two signs
		{2, 1, 4}, // one pulse in either coordinate, either sign
		{2, 2, 8}, // two in one place, or one in each
		{3, 1, 6}, // one pulse, three places, two signs
	}
	for _, c := range cases {
		if got := CombinationCount(c.n, c.k); got != c.want {
			t.Errorf("V(%d, %d) = %d, want %d", c.n, c.k, got, c.want)
		}
	}
}

// TestCombinationCountSaturates checks that an absurd request returns a ceiling rather than
// wrapping, since a wrapped count would make the index decoder read a plausible but wrong value.
func TestCombinationCountSaturates(t *testing.T) {
	got := CombinationCount(200, 200)
	if got != math.MaxUint32 {
		t.Errorf("V(200, 200) = %d, want it saturated at %d", got, uint32(math.MaxUint32))
	}
}

// TestPVQRoundTrip is the shape coder's gate.
//
// The indexing is a bijection between vectors and integers, so every vector with the right pulse
// count must survive. Small sizes are covered exhaustively, where an off-by-one in the walk down
// through the counts would show.
func TestPVQRoundTrip(t *testing.T) {
	// Every vector of n coordinates summing to k, for small n and k.
	var enumerate func(n, k int, prefix []int, visit func([]int))
	enumerate = func(n, k int, prefix []int, visit func([]int)) {
		if n == 0 {
			if k == 0 {
				visit(prefix)
			}
			return
		}
		for v := -k; v <= k; v++ {
			mag := v
			if mag < 0 {
				mag = -mag
			}
			enumerate(n-1, k-mag, append(prefix, v), visit)
		}
	}

	for n := 1; n <= 4; n++ {
		for k := 1; k <= 5; k++ {
			enumerate(n, k, nil, func(vec []int) {
				enc := rangecoder.NewEncoder(256)
				EncodePVQ(enc, vec, k)
				data := enc.Done()

				out := make([]int, n)
				dec := rangecoder.NewDecoder(data)
				DecodePVQ(dec, out, k)

				for i := range vec {
					if out[i] != vec[i] {
						t.Fatalf("n=%d k=%d: %v round-tripped as %v", n, k, vec, out)
					}
				}
			})
		}
	}
}

// TestPVQRoundTripLargeBands covers the band sizes a real stream uses.
func TestPVQRoundTripLargeBands(t *testing.T) {
	rng := rand.New(rand.NewPCG(13, 19))

	tried := 0
	for _, n := range []int{8, 16, 32, 48, 96, 176} {
		for _, k := range []int{1, 2, 5, 11, 24} {
			// A combination needing more than 32 bits is split by the caller and never reaches the
			// quantiser, so asking for one here would test a case the format excludes.
			if SplitRequired(n, k) {
				continue
			}
			tried++
			// Give each coordinate a fixed sign, then add unit pulses to it. Adding signed steps
			// directly can cancel against what a coordinate already holds, leaving a total other
			// than k and so a vector the codebook does not contain.
			vec := make([]int, n)
			signs := make([]int, n)
			for i := range signs {
				signs[i] = 1
				if rng.IntN(2) == 0 {
					signs[i] = -1
				}
			}
			for range k {
				i := rng.IntN(n)
				vec[i] += signs[i]
			}

			enc := rangecoder.NewEncoder(1 << 14)
			EncodePVQ(enc, vec, k)
			data := enc.Done()
			if enc.Overflowed() {
				t.Fatalf("n=%d k=%d: encoder overflowed", n, k)
			}

			out := make([]int, n)
			dec := rangecoder.NewDecoder(data)
			DecodePVQ(dec, out, k)

			for i := range vec {
				if out[i] != vec[i] {
					t.Fatalf("n=%d k=%d coordinate %d: got %d, want %d", n, k, i, out[i], vec[i])
				}
			}
		}
	}
	if tried < 10 {
		t.Errorf("only %d combinations were in range; the test is covering almost nothing", tried)
	}
}

// TestSplitRequiredMarksTheBoundary pins where the quantiser hands off to the split decoder.
//
// Below the boundary the codebook is addressable and the round trip holds; above it the count
// saturates, and a caller that pressed on regardless would decode a vector the codebook does not
// contain. That makes this the precondition of everything else in this file.
func TestSplitRequiredMarksTheBoundary(t *testing.T) {
	// Narrow bands never need splitting, however many pulses they carry.
	for _, k := range []int{1, 10, 100, MaxPulses} {
		if SplitRequired(1, k) {
			t.Errorf("a single coordinate with %d pulses was said to need splitting", k)
		}
	}
	// Wide bands with many pulses always do.
	for _, c := range [][2]int{{16, 24}, {48, 11}, {176, 5}, {176, 24}} {
		if !SplitRequired(c[0], c[1]) {
			t.Errorf("n=%d k=%d was said not to need splitting", c[0], c[1])
		}
	}
	// The boundary is where the count saturates, and it is monotone in k.
	for _, n := range []int{4, 16, 48, 176} {
		split := false
		for k := 1; k <= 40; k++ {
			needs := SplitRequired(n, k)
			if split && !needs {
				t.Errorf("n=%d: k=%d needs splitting but k=%d does not", n, k-1, k)
			}
			split = needs
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// TestPVQIndexIsABijection checks that every index in range maps to a distinct vector with the right
// pulse count, which is what makes the codebook complete.
func TestPVQIndexIsABijection(t *testing.T) {
	for n := 1; n <= 4; n++ {
		for k := 1; k <= 5; k++ {
			total := CombinationCount(n, k)
			seen := make(map[string]bool, total)
			table := newPVQTable(n, k)

			for i := range total {
				out := make([]int, n)
				decodePVQIndex(table, i, out, k)

				sum := 0
				for _, v := range out {
					sum += abs(v)
				}
				if sum != k {
					t.Fatalf("n=%d k=%d index %d gave %v, summing to %d", n, k, i, out, sum)
				}

				key := ""
				for _, v := range out {
					key += string(rune(v + 1000))
				}
				if seen[key] {
					t.Fatalf("n=%d k=%d: index %d repeated a vector %v", n, k, i, out)
				}
				seen[key] = true
			}
			if len(seen) != int(total) {
				t.Errorf("n=%d k=%d: %d distinct vectors from %d indices", n, k, len(seen), total)
			}
		}
	}
}

// TestPVQZeroPulsesIsSilence covers the band that received no allocation.
func TestPVQZeroPulsesIsSilence(t *testing.T) {
	out := make([]int, 16)
	for i := range out {
		out[i] = 99
	}
	dec := rangecoder.NewDecoder([]byte{0xFF, 0xFF})
	DecodePVQ(dec, out, 0)

	for i, v := range out {
		if v != 0 {
			t.Errorf("coordinate %d = %d, want 0", i, v)
		}
	}
}

// TestPulsesForBitsRespectsTheBudget pins the property the allocator depends on: a band never costs
// more than it was given.
func TestPulsesForBitsRespectsTheBudget(t *testing.T) {
	for _, n := range []int{1, 2, 4, 8, 16, 48, 176} {
		for _, bits := range []int{0, 8, 24, 64, 160, 400} {
			k := PulsesForBits(n, bits)
			if k < 0 {
				t.Fatalf("n=%d bits=%d gave %d pulses", n, bits, k)
			}
			if cost := PVQBits(n, k); cost > bits {
				t.Errorf("n=%d bits=%d: %d pulses cost %d, over budget", n, bits, k, cost)
			}
			// And one more pulse would not have fitted, or the search stopped early.
			if k < MaxPulses && PVQBits(n, k+1) <= bits {
				t.Errorf("n=%d bits=%d: %d pulses chosen but %d also fits", n, bits, k, k+1)
			}
		}
	}
}

// TestPVQBitsGrowsWithPulses pins the monotonicity the search bisects on.
func TestPVQBitsGrowsWithPulses(t *testing.T) {
	for _, n := range []int{1, 2, 8, 32} {
		prev := -1
		for k := range 30 {
			cost := PVQBits(n, k)
			if cost < prev {
				t.Fatalf("n=%d: %d pulses cost %d, less than %d for %d pulses", n, k, cost, prev, k-1)
			}
			prev = cost
		}
	}
}

func TestNormalise(t *testing.T) {
	vec := []int{3, -4, 0}
	out := make([]float32, 3)
	Normalise(vec, out)

	// The vector has length five, so the result is the direction alone.
	want := []float32{0.6, -0.8, 0}
	for i := range want {
		if math.Abs(float64(out[i]-want[i])) > 1e-6 {
			t.Errorf("coordinate %d = %v, want %v", i, out[i], want[i])
		}
	}

	var norm float64
	for _, v := range out {
		norm += float64(v) * float64(v)
	}
	if math.Abs(norm-1) > 1e-6 {
		t.Errorf("norm %v, want 1", norm)
	}
}

func TestNormaliseZeroVector(t *testing.T) {
	out := make([]float32, 4)
	for i := range out {
		out[i] = 7
	}
	Normalise([]int{0, 0, 0, 0}, out)
	for i, v := range out {
		if v != 0 {
			t.Errorf("coordinate %d = %v, want 0 for a zero vector", i, v)
		}
	}
}

func FuzzPVQRoundTrip(f *testing.F) {
	f.Add([]byte{1, 2, 3}, uint8(4), uint8(3))
	f.Add([]byte{0}, uint8(1), uint8(1))

	f.Fuzz(func(t *testing.T, magnitudes []byte, nRaw, kRaw uint8) {
		n := int(nRaw%32) + 1
		k := int(kRaw%24) + 1
		if len(magnitudes) == 0 {
			return
		}
		// The quantiser is only defined below the 32-bit codebook limit; above it the caller splits
		// the band instead. The fuzzer reaches that boundary quickly, so the precondition has to be
		// stated here rather than assumed.
		if SplitRequired(n, k) {
			return
		}

		// Build a vector with exactly k pulses from the fuzzer's bytes.
		vec := make([]int, n)
		left := k
		for i := 0; left > 0; i++ {
			idx := i % n
			take := min(left, int(magnitudes[i%len(magnitudes)])%4+1)
			if magnitudes[i%len(magnitudes)]&0x80 != 0 {
				vec[idx] -= take
			} else {
				vec[idx] += take
			}
			left -= take
		}
		sum := 0
		for _, v := range vec {
			sum += abs(v)
		}
		if sum != k {
			return // the construction did not land on k
		}

		enc := rangecoder.NewEncoder(1 << 14)
		EncodePVQ(enc, vec, k)
		data := enc.Done()
		if enc.Overflowed() {
			return
		}

		out := make([]int, n)
		DecodePVQ(rangecoder.NewDecoder(data), out, k)
		for i := range vec {
			if out[i] != vec[i] {
				t.Fatalf("n=%d k=%d: %v round-tripped as %v", n, k, vec, out)
			}
		}
	})
}

func BenchmarkDecodePVQ(b *testing.B) {
	const n, k = 48, 12
	vec := make([]int, n)
	left := k
	for i := 0; left > 0; i++ {
		vec[i%n]++
		left--
	}
	enc := rangecoder.NewEncoder(1 << 12)
	EncodePVQ(enc, vec, k)
	data := enc.Done()

	out := make([]int, n)
	b.ReportAllocs()
	for b.Loop() {
		DecodePVQ(rangecoder.NewDecoder(data), out, k)
	}
}

// TestMidpointDoesNotWrap covers the overflow that a fuzz seed exposed.
//
// Two codebook counts can each sit below the 32-bit ceiling while their sum crosses it. Adding them
// in 32 bits wraps to a small midpoint, the index walk takes the wrong branch at the first
// coordinate, and the vector decodes to something unrelated rather than failing.
func TestMidpointDoesNotWrap(t *testing.T) {
	const near = uint32(3 << 30) // two of these sum past the ceiling
	if got := midpoint(near, near); got != near {
		t.Errorf("midpoint(%d, %d) = %d, want %d", near, near, got, near)
	}
	if got := midpoint(math.MaxUint32, math.MaxUint32-1); got != math.MaxUint32-1 {
		t.Errorf("midpoint at the ceiling = %d, want %d", got, uint32(math.MaxUint32-1))
	}
}

// TestPVQRoundTripNearTheCodebookCeiling exercises the sizes where the midpoint sum overflows.
//
// These are legal bands: the codebook still fits in 32 bits, so the splitting rule does not apply and
// the quantiser must handle them. The plain round-trip tests use small n and k and never reach here.
func TestPVQRoundTripNearTheCodebookCeiling(t *testing.T) {
	tried := 0
	for n := 16; n <= 32; n++ {
		for k := 1; k <= 24; k++ {
			if SplitRequired(n, k) {
				continue
			}
			// Only the sizes whose midpoint sum crosses the ceiling are of interest here.
			a, b := CombinationCount(n-1, k), CombinationCount(n, k)
			if uint64(a)+uint64(b) <= math.MaxUint32 {
				continue
			}
			tried++

			vec := make([]int, n)
			for i := range k {
				vec[i%n]++ // all positive, so the magnitudes sum to k exactly
			}
			enc := rangecoder.NewEncoder(1 << 12)
			EncodePVQ(enc, vec, k)
			got := make([]int, n)
			DecodePVQ(rangecoder.NewDecoder(enc.Done()), got, k)

			if !slices.Equal(vec, got) {
				t.Fatalf("n=%d k=%d: %v round-tripped as %v", n, k, vec, got)
			}
		}
	}
	if tried == 0 {
		t.Fatal("no size reached the overflow region; the test verified nothing")
	}
	t.Logf("covered %d sizes whose midpoint sum exceeds 32 bits", tried)
}
