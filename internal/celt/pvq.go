package celt

import (
	"math"

	"github.com/faide/goxiph/internal/rangecoder"
)

// The pyramid vector quantiser codes a band's shape as a vector of N integers whose absolute values
// sum to K, then scales it to unit norm. Every such vector is enumerated, so the whole shape costs
// one uniformly distributed index and nothing else.
//
// Implemented from RFC 6716 section 4.3.4.2, which states both the recurrence and the indexing.

// CombinationCount returns V(n, k), the number of vectors of n integers whose absolute values sum
// to k.
//
// V(n, k) = V(n-1, k) + V(n, k-1) + V(n-1, k-1), with V(n, 0) = 1 and V(0, k) = 0 for k above zero.
// The three terms count the vectors whose first coordinate is zero, positive and negative in turn.
//
// The result saturates rather than wrapping. A legal stream never reaches the ceiling because the
// splitting rule keeps codebooks inside 32 bits, but a crafted one can ask for anything.
func CombinationCount(n, k int) uint32 {
	if n < 0 || k < 0 {
		return 0
	}
	if k == 0 {
		return 1
	}
	if n == 0 {
		return 0
	}

	// One row at a time: row[j] holds V(i, j) for the current i.
	row := make([]uint32, k+1)
	row[0] = 1 // V(0, 0)

	for i := 1; i <= n; i++ {
		prevDiag := row[0] // V(i-1, 0)
		row[0] = 1         // V(i, 0)
		for j := 1; j <= k; j++ {
			above := row[j] // V(i-1, j)
			left := row[j-1]
			row[j] = satAdd(satAdd(above, left), prevDiag)
			prevDiag = above
		}
	}
	return row[k]
}

// midpoint returns (a + b) / 2 without letting the sum wrap.
//
// Both counts can approach the 32-bit ceiling on their own, so adding them first overflows while the
// result still fits. A wrapped midpoint sends the index walk down the wrong branch and the vector
// comes back unrecognisable, which is how the fuzzer found this.
func midpoint(a, b uint32) uint32 {
	return uint32((uint64(a) + uint64(b)) / 2)
}

func satAdd(a, b uint32) uint32 {
	if a > math.MaxUint32-b {
		return math.MaxUint32
	}
	return a + b
}

// pvqTable holds V(n, k) for every n and k a single decode needs, so the inner loops are lookups.
type pvqTable struct {
	n, k int
	v    []uint32 // v[i*(k+1)+j] is V(i, j)
}

func newPVQTable(n, k int) *pvqTable {
	t := &pvqTable{n: n, k: k, v: make([]uint32, (n+1)*(k+1))}
	for i := range n + 1 {
		t.v[i*(k+1)] = 1 // V(i, 0)
	}
	for i := 1; i <= n; i++ {
		for j := 1; j <= k; j++ {
			t.v[i*(k+1)+j] = satAdd(satAdd(
				t.v[(i-1)*(k+1)+j],
				t.v[i*(k+1)+j-1]),
				t.v[(i-1)*(k+1)+j-1])
		}
	}
	return t
}

func (t *pvqTable) at(n, k int) uint32 {
	if n < 0 || k < 0 || n > t.n || k > t.k {
		return 0
	}
	return t.v[n*(t.k+1)+k]
}

// DecodePVQ reads one shape vector of n coordinates carrying k pulses into out.
//
// The vector is returned as integers; scaling to unit norm is the caller's, because the split
// decoder combines two halves before normalising either.
func DecodePVQ(d *rangecoder.Decoder, out []int, k int) {
	n := len(out)
	clear(out)
	if n == 0 || k == 0 {
		return
	}

	t := newPVQTable(n, k)
	i := d.DecodeUint(t.at(n, k))
	decodePVQIndex(t, i, out, k)
}

// decodePVQIndex converts a codeword index into its vector. RFC 6716 section 4.3.4.2.
//
// At each coordinate the index is split into a sign half and a magnitude, walking down through the
// counts until the remaining index fits.
func decodePVQIndex(t *pvqTable, i uint32, out []int, k int) {
	n := len(out)
	for j := range n {
		p := midpoint(t.at(n-j-1, k), t.at(n-j, k))

		sgn := 1
		if i >= p {
			sgn = -1
			i -= p
		}

		k0 := k
		p -= t.at(n-j-1, k)
		// The floor on k is what stops a crafted index spinning here. Below zero the counts read as
		// zero, so p would stop changing while the condition stayed true; a legal index never
		// reaches that point, so the guard costs nothing and bounds the loop for one that does.
		for p > i && k > 0 {
			k--
			p -= t.at(n-j-1, k)
		}

		out[j] = sgn * (k0 - k)
		i -= p
	}
}

// EncodePVQ writes a shape vector, whose absolute values must sum to k.
//
// It exists so the decoder can be checked by round trip: the indexing has no simpler inverse to
// compare against, and a vector that survives the trip pins both halves of it.
func EncodePVQ(e *rangecoder.Encoder, vec []int, k int) {
	n := len(vec)
	if n == 0 || k == 0 {
		return
	}

	t := newPVQTable(n, k)
	e.EncodeUint(encodePVQIndex(t, vec, k), t.at(n, k))
}

// encodePVQIndex is the inverse of decodePVQIndex.
func encodePVQIndex(t *pvqTable, vec []int, k int) uint32 {
	n := len(vec)
	var index uint32

	for j := range n {
		p := midpoint(t.at(n-j-1, k), t.at(n-j, k))

		// A zero coordinate takes the positive branch, because that is the one the decoder reaches
		// when the index falls below the split.
		if vec[j] < 0 {
			index += p
		}

		p -= t.at(n-j-1, k)
		magnitude := vec[j]
		if magnitude < 0 {
			magnitude = -magnitude
		}
		for range magnitude {
			k--
			p -= t.at(n-j-1, k)
		}

		index += p
	}
	return index
}

// PVQBits returns the cost of coding a shape of n coordinates with k pulses, in eighths of a bit.
//
// The codebook is enumerated, so the cost is the log of its size; the eighths come from the
// allocator working at that precision throughout.
func PVQBits(n, k int) int {
	return costOf(CombinationCount(n, k))
}

// SplitRequired reports whether a band of n coordinates carrying k pulses needs splitting.
//
// RFC 6716 section 4.3.4.4 caps a codebook at 32 bits so the index arithmetic stays in one word.
// Beyond that the band is halved and the process recurses, so the quantiser itself is never asked
// for a codebook it cannot address. A caller that ignores this hands the index decoder a saturated
// size, which reads a value the codebook does not contain.
func SplitRequired(n, k int) bool {
	return CombinationCount(n, k) == math.MaxUint32
}

// MaxPulses bounds the pulse count any band may carry.
//
// The allocator never asks for more: a band's capacity is bounded by its cap, and the splitting rule
// keeps codebooks inside 32 bits. The limit exists so a crafted allocation cannot drive the search
// arbitrarily far.
const MaxPulses = 256

// PulsesForBits returns the largest pulse count whose codebook fits in the given eighths of a bit.
//
// RFC 6716 section 4.3.4.1: the search takes the value nearest the allocation without exceeding it,
// so a band never overruns the capacity the allocator gave it.
//
// The scan is incremental rather than a bisection over recomputed counts. The counts for successive
// pulse values come from the same table, so walking them costs one table build; bisecting would
// rebuild it at every step, and for a narrow band whose codebook never reaches the ceiling that
// difference is the difference between fast and unusable.
func PulsesForBits(n, bits int) int {
	if n <= 0 || bits <= 0 {
		return 0
	}

	row := pvqRow(n, MaxPulses)
	best := 0
	for k := 1; k <= MaxPulses; k++ {
		if costOf(row[k]) > bits {
			break
		}
		best = k
	}
	return best
}

// pvqRow returns V(n, k) for every k from zero to maxK.
func pvqRow(n, maxK int) []uint32 {
	row := make([]uint32, maxK+1)
	row[0] = 1 // V(0, 0)

	for i := 1; i <= n; i++ {
		prevDiag := row[0]
		row[0] = 1
		for j := 1; j <= maxK; j++ {
			above := row[j]
			row[j] = satAdd(satAdd(above, row[j-1]), prevDiag)
			prevDiag = above
		}
	}
	return row
}

// costOf converts a codebook size to eighths of a bit.
func costOf(v uint32) int {
	if v <= 1 {
		return 0
	}
	return int(math.Ceil(math.Log2(float64(v)) * (1 << BitRes)))
}

// Normalise scales an integer shape vector to unit norm, writing the result into out.
func Normalise(vec []int, out []float32) {
	var sum float64
	for _, v := range vec {
		sum += float64(v) * float64(v)
	}
	if sum == 0 {
		clear(out)
		return
	}
	scale := 1 / math.Sqrt(sum)
	for i, v := range vec {
		out[i] = float32(float64(v) * scale)
	}
}
