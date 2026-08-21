package celt

import "math/bits"

// The allocator hands each band a budget in eighths of a bit, and the band has to turn that into a
// pulse count. The mapping is not smooth: only certain pulse counts are reachable, and the cost of
// each is the log of its codebook size. The cache holds that cost curve per band size.
//
// The reference ships the cache as a table. It is computed here instead, from the same recurrence
// the quantiser uses, and checked against that table entry by entry.
//
// Adapted from celt/rate.c, celt/rate.h and celt/cwrs.c.

// maxPseudo bounds the pulse index, and logMaxPseudo is the number of bisection steps needed to
// search it. From celt/rate.h.
const (
	maxPseudo    = 40
	logMaxPseudo = 6
)

// getPulses maps a pulse index to a pulse count.
//
// The scale is linear to eight and then geometric, which is what lets one byte of cache cover a
// range that reaches into the hundreds. RFC 6716 section 4.3.4.1 calls these pseudo-pulse counts.
func getPulses(i int) int {
	if i < 8 {
		return i
	}
	return (8 + (i & 7)) << uint((i>>3)-1)
}

// log2Frac returns the base-two logarithm of val with frac bits of fraction, rounded up.
//
// The rounding is what matters. The result sets how many bits a band is charged, so a decoder that
// rounded the other way would charge differently and desynchronise; this is the reference's
// procedure rather than a call to math.Log2.
func log2Frac(val uint32, frac int) int {
	l := bits.Len32(val)
	if val&(val-1) == 0 {
		// An exact power of two needs no rounding.
		return (l - 1) << uint(frac)
	}

	// Bring val into Q16, rounding up, without letting the shift overflow.
	if l > 16 {
		sh := uint(l - 16)
		val = (val >> sh) + ((val&((1<<sh)-1))+(1<<sh)-1)>>sh
	} else {
		val <<= uint(16 - l)
	}
	l = (l - 1) << uint(frac)

	// One iteration is always needed: the rounding above can have carried into the integer part.
	for {
		b := int(val >> 16)
		l += b << uint(frac)
		val = (val + uint32(b)) >> uint(b)
		val = (val*val + 0x7FFF) >> 15
		if frac <= 0 {
			break
		}
		frac--
	}
	if val > 0x8000 {
		l++
	}
	return l
}

// fitsIn32 reports whether V(n, k) fits in a 32-bit unsigned integer, from celt/rate.c.
//
// It bounds the cache: a band size stops gaining entries once the next pulse count would need a
// codebook the index arithmetic cannot address.
func fitsIn32(n, k int) bool {
	maxN := [15]int{32767, 32767, 32767, 1476, 283, 109, 60, 40, 29, 24, 20, 18, 16, 14, 13}
	maxK := [15]int{32767, 32767, 32767, 32767, 1172, 238, 95, 53, 36, 27, 22, 18, 16, 15, 13}
	if n >= 14 {
		if k >= 14 {
			return false
		}
		return n <= maxN[k]
	}
	return k <= maxK[n]
}

// pulseCache holds the cost curve for every distinct band size a mode uses.
//
// index maps a frame size and band to a run in bits; that run starts with the highest reachable
// pulse index and is followed by one cost per index. Bands that happen to be the same width share a
// run, which is why the index is indirect rather than a plain stride.
type pulseCache struct {
	index []int16 // (maxLM+2) * NumBands, -1 where the band has no coefficients to code
	bits  []byte
}

// maxLM is the largest frame-size shift, so the cache covers shifts zero through maxLM+1.
const maxLM = 3

// newPulseCache builds the cache for the standard band layout.
func newPulseCache() *pulseCache {
	const rows = maxLM + 2
	c := &pulseCache{index: make([]int16, rows*NumBands)}

	// halfWidth is the width the cache is keyed on: the split codes half a band at a time, so the
	// costs it needs are those of the halves.
	halfWidth := func(lm, band int) int {
		return (BandEdges[band+1] - BandEdges[band]) << uint(lm) >> 1
	}

	type entry struct{ n, k, at int }
	var entries []entry
	curr := 0

	for i := range rows {
		for j := range NumBands {
			n := halfWidth(i, j)
			c.index[i*NumBands+j] = -1

			// Reuse the run of any earlier band of the same size.
			found := false
			for k := 0; k <= i && !found; k++ {
				for m := 0; m < NumBands && (k != i || m < j); m++ {
					if n == halfWidth(k, m) {
						c.index[i*NumBands+j] = c.index[k*NumBands+m]
						found = true
						break
					}
				}
			}
			if found || n == 0 {
				continue
			}

			k := 0
			for k < maxPseudo && fitsIn32(n, getPulses(k+1)) {
				k++
			}
			entries = append(entries, entry{n: n, k: k, at: curr})
			c.index[i*NumBands+j] = int16(curr)
			curr += k + 1
		}
	}

	c.bits = make([]byte, curr)
	for _, e := range entries {
		c.bits[e.at] = byte(e.k)
		for j := 1; j <= e.k; j++ {
			c.bits[e.at+j] = byte(log2Frac(CombinationCount(e.n, getPulses(j)), BitRes) - 1)
		}
	}
	return c
}

// run returns the cost curve for a band at a frame size, or nil if the band codes nothing there.
func (c *pulseCache) run(lm, band int) []byte {
	at := c.index[(lm+1)*NumBands+band]
	if at < 0 {
		return nil
	}
	return c.bits[at:]
}

// bitsToPulses returns the pulse index whose cost is nearest the given budget without preferring to
// overshoot, by bisecting the cost curve. RFC 6716 section 4.3.4.1.
func (c *pulseCache) bitsToPulses(lm, band, budget int) int {
	cache := c.run(lm, band)
	if cache == nil {
		return 0
	}

	lo, hi := 0, int(cache[0])
	budget--
	for range logMaxPseudo {
		mid := (lo + hi + 1) >> 1
		if int(cache[mid]) >= budget {
			hi = mid
		} else {
			lo = mid
		}
	}

	// Take whichever end is closer. The low end reads as -1 rather than its cost, because index zero
	// means no pulses and costs nothing at all.
	below := -1
	if lo != 0 {
		below = int(cache[lo])
	}
	if budget-below <= int(cache[hi])-budget {
		return lo
	}
	return hi
}

// pulsesToBits returns what a pulse index costs, in eighths of a bit.
func (c *pulseCache) pulsesToBits(lm, band, pulses int) int {
	if pulses == 0 {
		return 0
	}
	cache := c.run(lm, band)
	if cache == nil {
		return 0
	}
	return int(cache[pulses]) + 1
}

// pulsesForBudget returns the pulse index a band can afford and what it costs, given the bits left
// in the frame.
//
// bitsToPulses takes whichever cost is nearest, which may overshoot; this steps back down until it
// fits. Without it a band could spend past the end of the frame and the range decoder would read
// symbols the encoder never wrote.
func (c *pulseCache) pulsesForBudget(lm, band, budget, remaining int) (q, cost int) {
	q = c.bitsToPulses(lm, band, budget)
	cost = c.pulsesToBits(lm, band, q)
	for remaining-cost < 0 && q > 0 {
		q--
		cost = c.pulsesToBits(lm, band, q)
	}
	return q, cost
}
