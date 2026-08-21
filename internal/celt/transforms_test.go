package celt

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestBitexactMatchesReference compares the fixed-point split arithmetic against the reference C.
//
// These three are the one part of CELT where an approximation is part of the format. The cosine
// feeds the mid/side gain split and the log-tangent feeds the bit split between the halves, so a
// value that is off by one allocates differently from every other decoder and the stream
// desynchronises. Being close is not enough, which is why this compares against the C rather than
// against math.Cos.
//
// The vectors come from testdata/gen_bitexact.c, built from the reference definitions. The angle
// domain is every multiple of 64 below 16384: itheta is i*16384/qn with qn capped at 256, so no
// finer angle exists, and the two endpoints are handled by the caller before the cosine is reached.
func TestBitexactMatchesReference(t *testing.T) {
	f, err := os.Open("testdata/bitexact_vectors.txt")
	if err != nil {
		t.Fatalf("reading vectors: %v", err)
	}
	defer f.Close()

	counts := map[string]int{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		num := func(i int) int {
			v, err := strconv.Atoi(fields[i])
			if err != nil {
				t.Fatalf("parsing %q: %v", fields[i], err)
			}
			return v
		}

		switch fields[0] {
		case "cos":
			x, want := num(1), num(2)
			if got := int(bitexactCos(int32(x))); got != want {
				t.Fatalf("bitexactCos(%d) = %d, reference has %d", x, got, want)
			}
		case "log2tan":
			isin, icos, want := num(1), num(2), num(3)
			if got := int(bitexactLog2Tan(int32(isin), int32(icos))); got != want {
				t.Fatalf("bitexactLog2Tan(%d, %d) = %d, reference has %d",
					isin, icos, got, want)
			}
		case "log2frac":
			val, frac, want := num(1), num(2), num(3)
			if got := log2Frac(uint32(val), frac); got != want {
				t.Fatalf("log2Frac(%d, %d) = %d, reference has %d", val, frac, got, want)
			}
		case "qn":
			n, b, off, cap_, st, want := num(1), num(2), num(3), num(4), num(5), num(6)
			if got := computeQn(n, b, off, cap_, st == 1); got != want {
				t.Fatalf("computeQn(N=%d b=%d offset=%d cap=%d stereo=%d) = %d, reference has %d",
					n, b, off, cap_, st, got, want)
			}
		default:
			t.Fatalf("unknown vector kind %q", fields[0])
		}
		counts[fields[0]]++
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}

	// A truncated or mis-parsed file would otherwise pass by checking nothing.
	for _, kind := range []string{"cos", "log2tan", "log2frac", "qn"} {
		if counts[kind] == 0 {
			t.Fatalf("no %s vectors were checked", kind)
		}
	}
	t.Logf("matched the reference on %d cosines, %d log-tangents, %d logarithms and %d qn cases",
		counts["cos"], counts["log2tan"], counts["log2frac"], counts["qn"])
}

// TestBitexactCosIsAFallingCosine checks the approximation behaves like the function it stands in
// for. The vector comparison above pins it to the reference; this says the reference is a cosine,
// so a wrong vector file could not quietly redefine it.
func TestBitexactCosIsAFallingCosine(t *testing.T) {
	prev := int32(math.MaxInt32)
	for x := int32(64); x < 16384; x += 64 {
		got := bitexactCos(x)
		if got >= prev {
			t.Fatalf("cos(%d) = %d did not fall below cos(%d) = %d", x, got, x-64, prev)
		}
		want := math.Cos(math.Pi / 2 * float64(x) / 16384)
		if math.Abs(float64(got)/32768-want) > 1e-3 {
			t.Fatalf("cos(%d) = %d (%.5f), want about %.5f", x, got, float64(got)/32768, want)
		}
		prev = got
	}
}

// TestHaarIsItsOwnInverse is the property the reshaping relies on: the decoder undoes a stage by
// applying another, so there is no separate inverse to get wrong.
func TestHaarIsItsOwnInverse(t *testing.T) {
	for _, tc := range []struct{ n0, stride int }{
		{8, 1}, {16, 1}, {8, 2}, {4, 4}, {32, 1}, {16, 4},
	} {
		n := tc.n0 * tc.stride
		orig := testShape(n)
		x := append([]float32(nil), orig...)

		haar1(x, tc.n0, tc.stride)

		moved := false
		for i := range x {
			if math.Abs(float64(x[i]-orig[i])) > 1e-6 {
				moved = true
			}
		}
		if !moved {
			t.Errorf("n0=%d stride=%d: the transform changed nothing", tc.n0, tc.stride)
		}

		haar1(x, tc.n0, tc.stride)
		for i := range x {
			if math.Abs(float64(x[i]-orig[i])) > 1e-5 {
				t.Fatalf("n0=%d stride=%d: coefficient %d %v -> %v",
					tc.n0, tc.stride, i, orig[i], x[i])
			}
		}
	}
}

func TestHaarPreservesNorm(t *testing.T) {
	for _, tc := range []struct{ n0, stride int }{{8, 1}, {16, 2}, {8, 4}} {
		x := testShape(tc.n0 * tc.stride)
		before := norm2(x)
		haar1(x, tc.n0, tc.stride)
		if after := norm2(x); math.Abs(after-before)/before > 1e-6 {
			t.Errorf("n0=%d stride=%d: norm %v -> %v", tc.n0, tc.stride, before, after)
		}
	}
}

// TestHadamardRoundTrips checks the two reorganisations are inverses, in both the permuted and
// plain orders.
func TestHadamardRoundTrips(t *testing.T) {
	for _, hadamard := range []bool{false, true} {
		for _, tc := range []struct{ n0, stride int }{
			{8, 2}, {4, 4}, {2, 8}, {16, 2}, {1, 16},
		} {
			orig := testShape(tc.n0 * tc.stride)
			x := append([]float32(nil), orig...)

			deinterleaveHadamard(x, tc.n0, tc.stride, hadamard)
			interleaveHadamard(x, tc.n0, tc.stride, hadamard)

			for i := range x {
				if x[i] != orig[i] {
					t.Fatalf("hadamard=%v n0=%d stride=%d: coefficient %d %v -> %v",
						hadamard, tc.n0, tc.stride, i, orig[i], x[i])
				}
			}
		}
	}
}

// TestHadamardIsAPermutation checks the reorganisation only moves coefficients, and that the
// permuted order differs from the plain one. Without the second half a broken ordery table would
// still round-trip.
func TestHadamardIsAPermutation(t *testing.T) {
	const n0, stride = 4, 4
	plain := make([]float32, n0*stride)
	for i := range plain {
		plain[i] = float32(i + 1)
	}
	permuted := append([]float32(nil), plain...)

	deinterleaveHadamard(plain, n0, stride, false)
	deinterleaveHadamard(permuted, n0, stride, true)

	seen := map[float32]int{}
	for _, v := range plain {
		seen[v]++
	}
	for i := range n0 * stride {
		if seen[float32(i+1)] != 1 {
			t.Fatalf("value %d appears %d times; not a permutation", i+1, seen[float32(i+1)])
		}
	}

	same := true
	for i := range plain {
		if plain[i] != permuted[i] {
			same = false
		}
	}
	if same {
		t.Error("the permuted order matched the plain one; the ordery table did nothing")
	}
}

// TestOrderyTableIsAPermutation checks each of the four block orders covers its range exactly once.
// A duplicated entry would drop a block and silently lose a coefficient.
func TestOrderyTableIsAPermutation(t *testing.T) {
	for _, stride := range []int{2, 4, 8, 16} {
		row := orderyTable[stride-2 : stride-2+stride]
		seen := make([]bool, stride)
		for _, v := range row {
			if v < 0 || v >= stride {
				t.Fatalf("stride %d: entry %d is out of range", stride, v)
			}
			if seen[v] {
				t.Fatalf("stride %d: entry %d appears twice", stride, v)
			}
			seen[v] = true
		}
	}
}

// TestComputeQnStaysEven covers the shape of the result rather than its value: the angle count is
// rounded up to an even number so the two halves can be balanced, and one means no angle is coded.
func TestComputeQnStaysEven(t *testing.T) {
	for _, n := range []int{2, 3, 4, 16, 100} {
		for b := 0; b <= 2048; b += 8 {
			for _, stereo := range []bool{false, true} {
				qn := computeQn(n, b, 4, 24, stereo)
				if qn != 1 && qn%2 != 0 {
					t.Fatalf("N=%d b=%d stereo=%v: qn = %d is neither one nor even",
						n, b, stereo, qn)
				}
				if qn < 1 || qn > 256 {
					t.Fatalf("N=%d b=%d stereo=%v: qn = %d is out of range", n, b, stereo, qn)
				}
			}
		}
	}
}

// TestComputeQnGrowsWithBudget checks that a larger budget buys a finer angle, and that the range
// is exercised across it. A constant result would satisfy the shape test above.
func TestComputeQnGrowsWithBudget(t *testing.T) {
	prev := 0
	distinct := map[int]bool{}
	for b := 0; b <= 2048; b += 8 {
		qn := computeQn(16, b, 4, 24, false)
		if qn < prev {
			t.Fatalf("b=%d: qn fell from %d to %d", b, prev, qn)
		}
		prev = qn
		distinct[qn] = true
	}
	if len(distinct) < 5 {
		t.Errorf("only %d distinct qn values across the budget range", len(distinct))
	}
	if prev != 256 {
		t.Errorf("the largest budget gave qn = %d, want the 256 cap", prev)
	}
}

// TestLCGRandMatchesReference pins the noise generator. The decoder has to produce the same noise
// the encoder assumed, so the constants are part of the format.
func TestLCGRandMatchesReference(t *testing.T) {
	// 1664525*seed + 1013904223, applied to a zero seed and then to its own output.
	want := []uint32{1013904223, 1196435762, 3519870697, 2868466484, 1649599747}
	seed := uint32(0)
	for i, w := range want {
		seed = lcgRand(seed)
		if seed != w {
			t.Fatalf("step %d: %d, want %d", i, seed, w)
		}
	}
}

func TestRenormaliseSetsTheGain(t *testing.T) {
	for _, gain := range []float32{1, 0.5, 2} {
		x := testShape(16)
		renormalise(x, gain)
		if got := math.Sqrt(norm2(x)); math.Abs(got-float64(gain)) > 1e-6 {
			t.Errorf("gain %v: norm came out %v", gain, got)
		}
	}

	// An all-zero band has no direction to scale, and must be left alone rather than divided by zero.
	zero := make([]float32, 8)
	renormalise(zero, 1)
	for i, v := range zero {
		if v != 0 {
			t.Errorf("coefficient %d became %v", i, v)
		}
	}
}

// TestBitInterleaveTableMatchesItsDerivation checks the table against the rule it encodes.
//
// The mask shrinks by half at each recombining step, so a pair of blocks becomes one that is
// occupied if either was. Deriving that here rather than trusting sixteen typed entries is the only
// check available: a wrong entry would lose a collapse and the anti-collapse pass would skip a band
// that needed refilling.
func TestBitInterleaveTableMatchesItsDerivation(t *testing.T) {
	for i := range 16 {
		var want byte
		if i&0b0011 != 0 {
			want |= 1
		}
		if i&0b1100 != 0 {
			want |= 2
		}
		if got := bitInterleaveTable[i]; got != want {
			t.Errorf("bitInterleaveTable[%04b] = %02b, want %02b", i, got, want)
		}
	}
}

// TestBitDeinterleaveTableMatchesItsDerivation checks the inverse rule: each bit of the mask becomes
// the pair of bits it was folded from, since a split block inherits its parent's occupancy.
func TestBitDeinterleaveTableMatchesItsDerivation(t *testing.T) {
	for i := range 16 {
		var want byte
		for b := range 4 {
			if i&(1<<b) != 0 {
				want |= 0b11 << (2 * b)
			}
		}
		if got := bitDeinterleaveTable[i]; got != want {
			t.Errorf("bitDeinterleaveTable[%04b] = %08b, want %08b", i, got, want)
		}
	}
}

// TestBitTablesAreConsistent checks that shrinking a mask and expanding it again recovers every
// block that was occupied. Expansion cannot recover which of a pair it was, so it widens.
func TestBitTablesAreConsistent(t *testing.T) {
	for i := range 16 {
		narrowed := bitInterleaveTable[i]
		widened := bitDeinterleaveTable[narrowed]
		if byte(i)&widened != byte(i) {
			t.Errorf("mask %04b shrank to %02b and expanded to %08b, losing a block",
				i, narrowed, widened)
		}
	}
}

// TestThetaOffsetUsesTheTwoPhaseCase covers the one branch: a two-sample stereo band takes the
// larger offset, which buys it a coarser angle.
func TestThetaOffsetUsesTheTwoPhaseCase(t *testing.T) {
	const pulseCap = 40

	if got, want := thetaOffset(pulseCap, 2, true), pulseCap/2-qthetaOffsetTwoPhase; got != want {
		t.Errorf("stereo N=2 offset = %d, want %d", got, want)
	}
	for _, tc := range []struct {
		n      int
		stereo bool
	}{{2, false}, {3, true}, {16, true}, {16, false}} {
		if got, want := thetaOffset(pulseCap, tc.n, tc.stereo), pulseCap/2-qthetaOffset; got != want {
			t.Errorf("N=%d stereo=%v offset = %d, want %d", tc.n, tc.stereo, got, want)
		}
	}

	// The coarser offset must never buy more angle steps, and must cost some somewhere. Checking one
	// budget is not enough: over most of the range both saturate at the same cap and the comparison
	// holds without the branch doing anything.
	differed := 0
	for cap2 := range 48 {
		for b := 0; b <= 512; b += 4 {
			twoPhase := computeQn(2, b, thetaOffset(cap2, 2, true), cap2, true)
			plain := computeQn(2, b, thetaOffset(cap2, 2, false), cap2, true)
			if twoPhase > plain {
				t.Fatalf("cap=%d b=%d: two-phase qn %d exceeds the plain %d",
					cap2, b, twoPhase, plain)
			}
			if twoPhase < plain {
				differed++
			}
		}
	}
	if differed == 0 {
		t.Fatal("the two offsets never gave different results; the branch was not exercised")
	}
	t.Logf("the two offsets differ in %d of the cases swept", differed)
}
