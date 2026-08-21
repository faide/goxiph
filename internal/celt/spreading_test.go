package celt

import (
	"math"
	"testing"

	"github.com/faide/goxiph/internal/rangecoder"
)

func norm2(x []float32) float64 {
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return s
}

func testShape(n int) []float32 {
	x := make([]float32, n)
	for i := range x {
		x[i] = float32(math.Sin(float64(i)*0.7 + 1))
	}
	return x
}

var rotationCases = []struct {
	n, blocks, k, spread int
}{
	{16, 1, 3, SpreadNormal},
	{32, 1, 4, SpreadAggressive},
	{24, 2, 3, SpreadLight},
	{100, 1, 10, SpreadNormal},
	{8, 1, 1, SpreadNormal},
	{176, 4, 8, SpreadNormal}, // wide enough for the second stride
}

// TestRotationPreservesNorm checks that the rotation is orthogonal.
//
// The shape a band carries is unit-norm by construction, and the energy is coded separately. A
// rotation that changed the norm would move energy between the two and the band would come out at
// the wrong level.
func TestRotationPreservesNorm(t *testing.T) {
	for _, tc := range rotationCases {
		x := testShape(tc.n)
		before := norm2(x)
		Rotate(x, tc.blocks, tc.k, tc.spread, true)
		after := norm2(x)

		if math.Abs(after-before)/before > 1e-6 {
			t.Errorf("n=%d B=%d k=%d spread=%d: norm %v -> %v",
				tc.n, tc.blocks, tc.k, tc.spread, before, after)
		}
	}
}

// TestRotationRoundTrips checks that the decoder's rotation undoes the encoder's.
//
// The two directions differ only in sign and order, so this is what pins the order down: getting it
// wrong still preserves the norm, and only the round trip notices.
func TestRotationRoundTrips(t *testing.T) {
	for _, tc := range rotationCases {
		orig := testShape(tc.n)
		x := append([]float32(nil), orig...)

		Rotate(x, tc.blocks, tc.k, tc.spread, true)
		Rotate(x, tc.blocks, tc.k, tc.spread, false)

		for i := range x {
			if math.Abs(float64(x[i]-orig[i])) > 1e-5 {
				t.Fatalf("n=%d B=%d k=%d spread=%d: coefficient %d %v -> %v",
					tc.n, tc.blocks, tc.k, tc.spread, i, orig[i], x[i])
			}
		}
	}
}

// TestRotationActuallySpreads is the point of the transform: a lone pulse must come out as many
// coefficients. A rotation by too small an angle would round-trip and preserve the norm while
// spreading nothing.
func TestRotationActuallySpreads(t *testing.T) {
	for _, spread := range []int{SpreadLight, SpreadNormal, SpreadAggressive} {
		x := make([]float32, 32)
		x[0] = 1
		Rotate(x, 1, 1, spread, false)

		nonZero := 0
		for _, v := range x {
			if math.Abs(float64(v)) > 1e-6 {
				nonZero++
			}
		}
		if nonZero < len(x)/2 {
			t.Errorf("spread=%d: one pulse reached only %d of %d coefficients",
				spread, nonZero, len(x))
		}
		if x[0] > 0.999 {
			t.Errorf("spread=%d: the pulse stayed at %v, so nothing was rotated", spread, x[0])
		}
	}
}

// TestRotationSkipsDenseBands covers the 2*K >= N cutoff of celt/vq.c. A band with a pulse for every
// other coefficient is already spread, and rotating it would only blur it.
func TestRotationSkipsDenseBands(t *testing.T) {
	x := testShape(16)
	orig := append([]float32(nil), x...)

	Rotate(x, 1, 8, SpreadAggressive, false) // 2*8 == 16
	for i := range x {
		if x[i] != orig[i] {
			t.Fatalf("coefficient %d moved from %v to %v in a dense band", i, orig[i], x[i])
		}
	}

	// One pulse fewer is below the cutoff and must rotate.
	Rotate(x, 1, 7, SpreadAggressive, false)
	if x[0] == orig[0] {
		t.Error("a band just below the cutoff was not rotated")
	}
}

func TestRotationSkipsSpreadNone(t *testing.T) {
	x := testShape(32)
	orig := append([]float32(nil), x...)
	Rotate(x, 1, 1, SpreadNone, false)
	for i := range x {
		if x[i] != orig[i] {
			t.Fatalf("coefficient %d moved with spreading off", i)
		}
	}
}

// TestRotationStaysWithinItsBlock checks the interleaving: each block rotates on its own, so energy
// planted in one must not leak into another.
func TestRotationStaysWithinItsBlock(t *testing.T) {
	const n, blocks = 32, 2
	x := make([]float32, n)
	for i := range n / blocks {
		x[i] = float32(math.Sin(float64(i)))
	}

	Rotate(x, blocks, 3, SpreadNormal, false)
	for i := n / blocks; i < n; i++ {
		if x[i] != 0 {
			t.Fatalf("coefficient %d in the empty block became %v", i, x[i])
		}
	}
}

// TestSpreadDecisionDistribution checks each spreading amount is reachable and that the decode is the
// inverse of the encode.
func TestSpreadDecisionDistribution(t *testing.T) {
	for want := range 4 {
		enc := rangecoder.NewEncoder(64)
		enc.EncodeICDF(want, spreadICDF, 5)
		data := enc.Done()

		dec := rangecoder.NewDecoder(data)
		if got := DecodeSpread(dec, len(data)*8); got != want {
			t.Errorf("spread %d decoded as %d", want, got)
		}
	}
}

// TestSpreadDecisionSkippedWhenShort covers the four-bit guard: with no room for the symbol the
// decoder must assume normal rather than read one and fall out of step.
func TestSpreadDecisionSkippedWhenShort(t *testing.T) {
	enc := rangecoder.NewEncoder(64)
	enc.EncodeICDF(SpreadNone, spreadICDF, 5)
	data := enc.Done()

	dec := rangecoder.NewDecoder(data)
	before := dec.Tell()
	if got := DecodeSpread(dec, before+3); got != SpreadNormal {
		t.Errorf("with three bits left, spread = %d, want SpreadNormal", got)
	}
	if dec.Tell() != before {
		t.Error("the skipped symbol still consumed bits")
	}
}

func TestCollapseMask(t *testing.T) {
	tests := []struct {
		name   string
		pulses []int
		blocks int
		want   uint32
	}{
		{"single block is never collapsed", []int{0, 0, 0, 0}, 1, 0b1},
		{"all blocks occupied", []int{1, 0, 0, 1, 0, 1}, 3, 0b111},
		{"middle block empty", []int{1, 0, 0, 0, 0, 1}, 3, 0b101},
		{"only the first", []int{2, 0, 0, 0}, 2, 0b01},
		{"only the last", []int{0, 0, 0, -3}, 2, 0b10},
		{"a negative pulse still occupies its block", []int{-1, 0}, 2, 0b01},
		{"nothing at all", []int{0, 0, 0, 0}, 2, 0b00},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CollapseMask(tt.pulses, tt.blocks); got != tt.want {
				t.Errorf("mask = %04b, want %04b", got, tt.want)
			}
		})
	}
}

// TestCollapseStaysVisibleThroughRotation checks that an empty block is still empty afterwards.
//
// The mask reads the pulse vector, and the rotation works inside a block, so neither can turn a
// collapsed block into an occupied one. A rotation that reached across blocks would refill the empty
// one with energy and the anti-collapse pass would then have nothing to correct.
func TestCollapseStaysVisibleThroughRotation(t *testing.T) {
	const n, blocks = 32, 2
	pulses := make([]int, n)
	pulses[0] = 1 // one pulse, in the first block only

	if got := CollapseMask(pulses, blocks); got != 0b01 {
		t.Fatalf("mask = %02b, want 01", got)
	}

	shape := make([]float32, n)
	Normalise(pulses, shape)
	Rotate(shape, blocks, 1, SpreadNormal, false)

	for i := n / blocks; i < n; i++ {
		if shape[i] != 0 {
			t.Fatalf("rotation put %v into the collapsed block at %d", shape[i], i)
		}
	}
	// The occupied block must have spread, or the comparison above proves nothing.
	if shape[1] == 0 {
		t.Error("the occupied block did not spread; the test would pass without rotation")
	}
}
