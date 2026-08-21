package celt

import (
	"math"
	"testing"
)

// TestGetPulsesIsLinearThenGeometric covers the pseudo-pulse scale: counts up to eight are
// themselves, and above that the step doubles every eight indices. That is what lets one byte of
// cache cover counts into the hundreds.
func TestGetPulsesIsLinearThenGeometric(t *testing.T) {
	for i := range 8 {
		if got := getPulses(i); got != i {
			t.Errorf("getPulses(%d) = %d, want %d", i, got, i)
		}
	}
	for _, tc := range []struct{ i, want int }{
		{8, 8}, {9, 9}, {15, 15}, {16, 16}, {17, 18}, {23, 30}, {24, 32}, {31, 60}, {32, 64},
	} {
		if got := getPulses(tc.i); got != tc.want {
			t.Errorf("getPulses(%d) = %d, want %d", tc.i, got, tc.want)
		}
	}

	// The scale must never go backwards, or the cache bisection would be searching an unsorted curve.
	prev := -1
	for i := range maxPseudo + 1 {
		got := getPulses(i)
		if got <= prev {
			t.Fatalf("getPulses(%d) = %d did not exceed %d", i, got, prev)
		}
		prev = got
	}
}

// TestLog2FracIsCloseToTheRealLogarithm checks the fixed-point logarithm against the function it
// approximates. The vector comparison in TestBitexactMatchesReference pins it to the reference; this
// says the reference is a logarithm, so a bad vector file could not redefine it.
//
// It rounds up at its intermediate steps, so it never falls below the exact ceiling and can sit one
// step above it. That asymmetry is the point: it is why costOf defers to this rather than rounding
// math.Log2 itself.
func TestLog2FracIsCloseToTheRealLogarithm(t *testing.T) {
	above := 0
	for v := uint32(1); v < 100000; v += 7 {
		got := log2Frac(v, BitRes)
		exact := math.Ceil(math.Log2(float64(v)) * (1 << BitRes))

		if float64(got) < exact {
			t.Fatalf("log2Frac(%d) = %d, below the exact ceiling %v", v, got, exact)
		}
		if float64(got) > exact+1 {
			t.Fatalf("log2Frac(%d) = %d, more than one step above the ceiling %v", v, got, exact)
		}
		if float64(got) > exact {
			above++
		}
	}
	if above == 0 {
		t.Error("log2Frac never exceeded the exact ceiling; costOf could have used math.Log2")
	}
	t.Logf("log2Frac sat one step above the exact ceiling in %d cases", above)
}

func TestLog2FracOnPowersOfTwo(t *testing.T) {
	for e := range 32 {
		v := uint32(1) << uint(e)
		if got, want := log2Frac(v, BitRes), e<<BitRes; got != want {
			t.Errorf("log2Frac(2^%d) = %d, want %d", e, got, want)
		}
	}
}

// TestLog2FracIsMonotonic guards the bisection in bitsToPulses, which assumes the cost curve rises.
func TestLog2FracIsMonotonic(t *testing.T) {
	prev := log2Frac(1, BitRes)
	for v := uint32(2); v < 200000; v++ {
		got := log2Frac(v, BitRes)
		if got < prev {
			t.Fatalf("log2Frac(%d) = %d fell below log2Frac(%d) = %d", v, got, v-1, prev)
		}
		prev = got
	}
}

// TestFitsIn32AgreesWithTheRecurrence cross-checks the reference's threshold tables against the
// saturating count the quantiser uses. They are two independent statements of the same bound, so a
// mistranscribed threshold shows up here.
func TestFitsIn32AgreesWithTheRecurrence(t *testing.T) {
	checked, disagreed := 0, 0
	for n := 1; n <= 200; n++ {
		for k := 1; k <= 200; k++ {
			table := fitsIn32(n, k)
			recurrence := CombinationCount(n, k) != math.MaxUint32
			checked++
			if table != recurrence {
				if disagreed < 5 {
					t.Errorf("V(%d,%d): fitsIn32 says %v, the recurrence says %v",
						n, k, table, recurrence)
				}
				disagreed++
			}
		}
	}
	if disagreed != 0 {
		t.Fatalf("%d of %d disagreed", disagreed, checked)
	}
	t.Logf("the threshold tables and the recurrence agree on all %d pairs", checked)
}

// TestPulseCacheRoundTripsToTheSameCost checks the two accessors agree.
//
// It is the cost that round-trips, not the index. A one-sample band codes only a sign, so every
// pulse count costs the same and the search returns the cheapest index reaching that cost; asking
// for the index back would be asking it to distinguish counts the format cannot.
func TestPulseCacheRoundTripsToTheSameCost(t *testing.T) {
	c := newPulseCache()
	checked, collapsed := 0, 0
	for lm := range maxLM + 1 {
		for band := range NumBands {
			run := c.run(lm, band)
			if run == nil {
				continue
			}
			for q := 1; q <= int(run[0]); q++ {
				cost := c.pulsesToBits(lm, band, q)
				got := c.bitsToPulses(lm, band, cost)

				if back := c.pulsesToBits(lm, band, got); back != cost {
					t.Fatalf("LM=%d band=%d: %d pulses cost %d, which bought %d costing %d",
						lm, band, q, cost, got, back)
				}
				if got > q {
					t.Fatalf("LM=%d band=%d: %d pulses cost %d, which bought a larger %d",
						lm, band, q, cost, got)
				}
				if got != q {
					collapsed++
				}
				checked++
			}
		}
	}
	if checked == 0 {
		t.Fatal("no cache entries were exercised")
	}
	t.Logf("round-tripped %d entries; %d had a cheaper index at the same cost", checked, collapsed)
}

// TestBitsToPulsesTakesTheNearestCost is the property the bisection has to establish. Checking it
// against a scan of the whole curve is what catches an off-by-one in the search, which a
// monotonicity check alone would not.
func TestBitsToPulsesTakesTheNearestCost(t *testing.T) {
	c := newPulseCache()
	// Index zero means no pulses, which the reference treats as costing less than nothing so that a
	// budget of zero prefers it outright.
	costAt := func(run []byte, i int) int {
		if i == 0 {
			return -1
		}
		return int(run[i])
	}

	for lm := range maxLM + 1 {
		for band := range NumBands {
			run := c.run(lm, band)
			if run == nil {
				continue
			}
			for budget := 0; budget < 400; budget++ {
				q := c.bitsToPulses(lm, band, budget)
				want := budget - 1
				got := abs(costAt(run, q) - want)

				for other := 0; other <= int(run[0]); other++ {
					if d := abs(costAt(run, other) - want); d < got {
						t.Fatalf("LM=%d band=%d budget=%d: chose %d at distance %d, but %d is %d away",
							lm, band, budget, q, got, other, d)
					}
				}
			}
		}
	}
}

// TestPulsesForBudgetNeverBusts covers the back-off. The nearest cost may overshoot, and a band that
// spent past the end of the frame would leave the range decoder reading symbols that were never
// written.
func TestPulsesForBudgetNeverBusts(t *testing.T) {
	c := newPulseCache()
	backedOff := 0
	for lm := range maxLM + 1 {
		for band := range NumBands {
			if c.run(lm, band) == nil {
				continue
			}
			for budget := 0; budget < 400; budget += 3 {
				for _, remaining := range []int{budget, budget / 2, 0, 4000} {
					q, cost := c.pulsesForBudget(lm, band, budget, remaining)
					if q > 0 && cost > remaining {
						t.Fatalf("LM=%d band=%d budget=%d remaining=%d: %d pulses cost %d",
							lm, band, budget, remaining, q, cost)
					}
					if q != c.bitsToPulses(lm, band, budget) {
						backedOff++
					}
				}
			}
		}
	}
	if backedOff == 0 {
		t.Error("the back-off never triggered; the loop is untested")
	}
	t.Logf("the back-off reduced the pulse count in %d cases", backedOff)
}

// TestPulseCacheCostsRise checks each cost curve is sorted, which the bisection assumes.
func TestPulseCacheCostsRise(t *testing.T) {
	c := newPulseCache()
	seen := map[int]bool{}
	for lm := range maxLM + 2 {
		for band := range NumBands {
			at := c.index[lm*NumBands+band]
			if at < 0 || seen[int(at)] {
				continue
			}
			seen[int(at)] = true

			run := c.bits[at:]
			for q := 2; q <= int(run[0]); q++ {
				if run[q] < run[q-1] {
					t.Fatalf("run at %d: cost fell from %d to %d at index %d",
						at, run[q-1], run[q], q)
				}
			}
		}
	}
	if len(seen) < 5 {
		t.Errorf("only %d distinct runs; the cache is not being shared as expected", len(seen))
	}
	t.Logf("%d distinct cost curves, all rising", len(seen))
}

// TestPulseCacheSharesRunsBetweenEqualWidths covers the indirection: bands that come out the same
// width share one run, which is why the index is a lookup rather than a stride.
func TestPulseCacheSharesRunsBetweenEqualWidths(t *testing.T) {
	c := newPulseCache()
	widthOf := func(lm, band int) int {
		return (BandEdges[band+1] - BandEdges[band]) << uint(lm) >> 1
	}

	byWidth := map[int]int16{}
	shared := 0
	for lm := range maxLM + 2 {
		for band := range NumBands {
			at := c.index[lm*NumBands+band]
			w := widthOf(lm, band)
			if at < 0 {
				if w != 0 {
					t.Errorf("LM=%d band=%d has width %d but no cache entry", lm, band, w)
				}
				continue
			}
			if prev, ok := byWidth[w]; ok {
				if prev != at {
					t.Errorf("width %d maps to both run %d and run %d", w, prev, at)
				}
				shared++
			} else {
				byWidth[w] = at
			}
		}
	}
	if shared == 0 {
		t.Error("no run was shared; the width lookup did nothing")
	}
	t.Logf("%d band-and-size pairs reused an existing run", shared)
}

// TestPulseCacheMissingBandsAreZeroWidth checks the one reason a band has no entry: at the shortest
// frame the narrow low bands halve to nothing, so there is no half-band to code.
func TestPulseCacheMissingBandsAreZeroWidth(t *testing.T) {
	c := newPulseCache()
	missing := 0
	for band := range NumBands {
		if c.index[band] < 0 {
			missing++
			if w := (BandEdges[band+1] - BandEdges[band]) >> 1; w != 0 {
				t.Errorf("band %d has no entry but halves to width %d", band, w)
			}
		}
	}
	if missing != 8 {
		t.Errorf("%d bands lack an entry at the shortest frame, want the 8 of width one", missing)
	}
}
