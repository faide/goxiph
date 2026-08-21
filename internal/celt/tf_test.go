package celt

import (
	"testing"

	"github.com/faide/goxiph/internal/rangecoder"
)

// TestTFSelectTableKeepsMergesToTransientFrames is the invariant the band reshaping depends on.
//
// A positive change merges blocks together, which needs blocks to merge. Only a transient frame has
// more than one, and the non-transient half of the table is what guarantees a long-block frame is
// never asked to halve its single block.
func TestTFSelectTableKeepsMergesToTransientFrames(t *testing.T) {
	for lm := range 4 {
		for entry := range 4 {
			if got := tfSelectTable[lm][entry]; got > 0 {
				t.Errorf("LM=%d non-transient entry %d is %d, which would merge blocks",
					lm, entry, got)
			}
		}
		// A transient frame may merge, but never more times than it has blocks to merge.
		for entry := 4; entry < 8; entry++ {
			if got := int(tfSelectTable[lm][entry]); got > lm {
				t.Errorf("LM=%d transient entry %d is %d, more than the %d available halvings",
					lm, entry, got, lm)
			}
		}
	}
}

// TestDecodeTFResolutionRoundTrips checks a planted run of flags comes back, across frame sizes and
// both transient settings.
func TestDecodeTFResolutionRoundTrips(t *testing.T) {
	for _, lm := range []int{0, 1, 2, 3} {
		for _, transient := range []bool{false, true} {
			for _, tfSelect := range []int{0, 1} {
				flags := make([]int, NumBands)
				for i := range flags {
					flags[i] = (i / 3) % 2
				}

				enc := rangecoder.NewEncoder(1 << 12)
				logp := uint32(4)
				if transient {
					logp = 2
				}
				curr, changed := 0, 0
				for i := range NumBands {
					enc.EncodeBitLogp(flags[i]^curr, logp)
					curr = flags[i]
					changed |= curr
					logp = 5
					if transient {
						logp = 4
					}
				}
				base := 0
				if transient {
					base = 4
				}
				wantSelect := 0
				if lm > 0 && tfSelectTable[lm][base+changed] != tfSelectTable[lm][base+2+changed] {
					enc.EncodeBitLogp(tfSelect, 1)
					wantSelect = tfSelect
				}
				data := enc.Done()

				got := make([]int, NumBands)
				DecodeTFResolution(rangecoder.NewDecoder(data), got, 0, NumBands, lm,
					transient, len(data)*8)

				for i := range NumBands {
					want := int(tfSelectTable[lm][base+2*wantSelect+flags[i]])
					if got[i] != want {
						t.Fatalf("LM=%d transient=%v select=%d band %d: %d, want %d",
							lm, transient, tfSelect, i, got[i], want)
					}
				}
			}
		}
	}
}

// TestDecodeTFResolutionDegradesWhenShort covers the budget guard: with no room for a flag the
// decoder must carry the previous band's setting rather than read one and fall out of step.
func TestDecodeTFResolutionDegradesWhenShort(t *testing.T) {
	for _, budget := range []int{0, 1, 3, 8, 20, 60} {
		for _, lm := range []int{0, 3} {
			got := make([]int, NumBands)
			dec := rangecoder.NewDecoder([]byte{0x55, 0xAA, 0x33, 0x0F})
			DecodeTFResolution(dec, got, 0, NumBands, lm, true, budget)

			base := 4
			for i, v := range got {
				ok := false
				for entry := range 4 {
					if v == int(tfSelectTable[lm][base+entry]) {
						ok = true
					}
				}
				if !ok {
					t.Fatalf("budget %d LM=%d band %d: %d is not a table value", budget, lm, i, v)
				}
			}
			if dec.Tell() > budget+16 {
				t.Errorf("budget %d: read %d bits", budget, dec.Tell())
			}
		}
	}
}

// TestDecodeTFResolutionRunsAreCheap checks the running exclusive-or does its job: a frame where
// every band shares a setting must cost less than one where they alternate.
func TestDecodeTFResolutionRunsAreCheap(t *testing.T) {
	cost := func(flags []int) int {
		enc := rangecoder.NewEncoder(1 << 12)
		curr := 0
		logp := uint32(2)
		for i := range NumBands {
			enc.EncodeBitLogp(flags[i]^curr, logp)
			curr = flags[i]
			logp = 4
		}
		return enc.TellFrac()
	}

	flat := make([]int, NumBands)
	alternating := make([]int, NumBands)
	for i := range alternating {
		alternating[i] = i % 2
	}
	if cost(flat) >= cost(alternating) {
		t.Errorf("a constant setting cost %d eighths, no less than the alternating %d",
			cost(flat), cost(alternating))
	}
}
