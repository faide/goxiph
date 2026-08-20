package flac

import (
	"errors"
	"testing"

	"github.com/faide/goxiph/internal/bitio"
)

// subframeBuilder constructs subframe bitstreams so the decoder can be driven through paths that no
// reference encoder in the corpus produces.
//
// Counting which paths real fixtures reach showed fixed order 0, 5-bit Rice parameters and escaped
// partitions never running. Coaxing ffmpeg into emitting them is guesswork; building them here is
// exact.
type subframeBuilder struct{ w *bitio.MSBWriter }

func newSubframeBuilder() *subframeBuilder {
	return &subframeBuilder{w: bitio.NewMSBWriter()}
}

// header writes the subframe header. wasted of 0 clears the flag.
func (b *subframeBuilder) header(kind uint64, wasted int) *subframeBuilder {
	_ = b.w.Write(0, 1)
	_ = b.w.Write(kind, 6)
	if wasted == 0 {
		_ = b.w.Write(0, 1)
		return b
	}
	_ = b.w.Write(1, 1)
	_ = b.w.WriteUnary(wasted - 1)
	return b
}

func (b *subframeBuilder) signed(v int64, bits uint) *subframeBuilder {
	_ = b.w.Write(uint64(v)&(1<<bits-1), bits)
	return b
}

// rice writes a residual as a single Rice-coded partition.
func (b *subframeBuilder) rice(values []int32, param uint, fiveBit bool) *subframeBuilder {
	if fiveBit {
		_ = b.w.Write(1, 2)
	} else {
		_ = b.w.Write(0, 2)
	}
	_ = b.w.Write(0, 4) // partition order 0

	paramBits := uint(4)
	if fiveBit {
		paramBits = 5
	}
	_ = b.w.Write(uint64(param), paramBits)

	for _, v := range values {
		folded := uint32(v) << 1
		if v < 0 {
			folded = uint32(-v)<<1 - 1
		}
		_ = b.w.WriteUnary(int(folded >> param))
		_ = b.w.Write(uint64(folded&(1<<param-1)), param)
	}
	return b
}

// escaped writes a residual as a single escaped partition of the given width.
func (b *subframeBuilder) escaped(values []int32, width uint, fiveBit bool) *subframeBuilder {
	if fiveBit {
		_ = b.w.Write(1, 2)
		_ = b.w.Write(0, 4)
		_ = b.w.Write(0b11111, 5)
	} else {
		_ = b.w.Write(0, 2)
		_ = b.w.Write(0, 4)
		_ = b.w.Write(0b1111, 4)
	}
	_ = b.w.Write(uint64(width), 5)
	for _, v := range values {
		if width > 0 {
			_ = b.w.Write(uint64(v)&(1<<width-1), width)
		}
	}
	return b
}

func (b *subframeBuilder) decode(t *testing.T, n int, depth uint) []int32 {
	t.Helper()
	// Pad so a read near the end of the final byte still succeeds.
	_ = b.w.Write(0, 32)
	out := make([]int32, n)
	if err := readSubframe(bitio.NewMSBReader(b.w.Bytes()), out, depth); err != nil {
		t.Fatalf("readSubframe: %v", err)
	}
	return out
}

func wantSamples(t *testing.T, got []int32, want ...int32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d samples, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestConstantSubframe(t *testing.T) {
	b := newSubframeBuilder().header(0, 0)
	b.signed(-1234, 16)
	wantSamples(t, b.decode(t, 4, 16), -1234, -1234, -1234, -1234)
}

func TestVerbatimSubframe(t *testing.T) {
	b := newSubframeBuilder().header(1, 0)
	for _, v := range []int64{7, -8, 0, 3} {
		b.signed(v, 8)
	}
	wantSamples(t, b.decode(t, 4, 8), 7, -8, 0, 3)
}

// TestFixedPredictorOrders walks every fixed predictor, order 0 included. Order 0 predicts nothing,
// so each residual is the sample itself; no fixture in the corpus uses it.
func TestFixedPredictorOrders(t *testing.T) {
	t.Run("order 0", func(t *testing.T) {
		b := newSubframeBuilder().header(8, 0)
		b.rice([]int32{5, -3, 0, 12}, 4, false)
		wantSamples(t, b.decode(t, 4, 16), 5, -3, 0, 12)
	})

	t.Run("order 1", func(t *testing.T) {
		// Warm-up 10, then residuals 1,1,1 give 11,12,13.
		b := newSubframeBuilder().header(9, 0)
		b.signed(10, 16)
		b.rice([]int32{1, 1, 1}, 2, false)
		wantSamples(t, b.decode(t, 4, 16), 10, 11, 12, 13)
	})

	t.Run("order 2", func(t *testing.T) {
		// Prediction 2*a(n-1) - a(n-2) continues an arithmetic run exactly.
		b := newSubframeBuilder().header(10, 0)
		b.signed(10, 16)
		b.signed(20, 16)
		b.rice([]int32{0, 0}, 2, false)
		wantSamples(t, b.decode(t, 4, 16), 10, 20, 30, 40)
	})

	t.Run("order 3", func(t *testing.T) {
		b := newSubframeBuilder().header(11, 0)
		for _, v := range []int64{1, 4, 9} {
			b.signed(v, 16)
		}
		b.rice([]int32{0, 0}, 2, false)
		// 3*9 - 3*4 + 1 = 16, then 3*16 - 3*9 + 4 = 25: the squares.
		wantSamples(t, b.decode(t, 5, 16), 1, 4, 9, 16, 25)
	})

	t.Run("order 4", func(t *testing.T) {
		b := newSubframeBuilder().header(12, 0)
		for _, v := range []int64{0, 0, 0, 0} {
			b.signed(v, 16)
		}
		b.rice([]int32{7, 0}, 2, false)
		wantSamples(t, b.decode(t, 6, 16), 0, 0, 0, 0, 7, 28)
	})
}

// TestLPCSubframe drives a linear predictor with a known coefficient set.
func TestLPCSubframe(t *testing.T) {
	// Order 2, coefficients [2, -1] with shift 0: the same prediction as fixed order 2.
	b := newSubframeBuilder().header(33, 0)
	b.signed(10, 16)
	b.signed(20, 16)
	b.signed(4-1, 4) // coefficient precision 4 bits
	b.signed(0, 5)   // shift 0
	b.signed(2, 4)   // coefficient for a(n-1)
	b.signed(-1, 4)  // coefficient for a(n-2)
	b.rice([]int32{0, 0}, 2, false)
	wantSamples(t, b.decode(t, 4, 16), 10, 20, 30, 40)
}

// TestLPCShiftIsApplied checks that the prediction right shift divides the accumulated sum.
func TestLPCShiftIsApplied(t *testing.T) {
	// Coefficient 4 with shift 2 is a unit predictor: 4*a(n-1) >> 2 == a(n-1).
	b := newSubframeBuilder().header(32, 0)
	b.signed(9, 16)
	b.signed(4-1, 4)
	b.signed(2, 5)
	b.signed(4, 4)
	b.rice([]int32{0, 0, 0}, 2, false)
	wantSamples(t, b.decode(t, 4, 16), 9, 9, 9, 9)
}

func TestLPCRejectsNegativeShift(t *testing.T) {
	b := newSubframeBuilder().header(32, 0)
	b.signed(1, 16)
	b.signed(4-1, 4)
	b.signed(-1, 5) // shift must not be negative
	b.signed(1, 4)
	b.rice([]int32{0}, 2, false)
	_ = b.w.Write(0, 32)

	out := make([]int32, 2)
	if err := readSubframe(bitio.NewMSBReader(b.w.Bytes()), out, 16); !errors.Is(err, ErrBadFrame) {
		t.Errorf("got %v, want ErrBadFrame", err)
	}
}

func TestLPCRejectsForbiddenPrecision(t *testing.T) {
	b := newSubframeBuilder().header(32, 0)
	b.signed(1, 16)
	_ = b.w.Write(0b1111, 4) // forbidden
	_ = b.w.Write(0, 32)

	out := make([]int32, 2)
	if err := readSubframe(bitio.NewMSBReader(b.w.Bytes()), out, 16); !errors.Is(err, ErrBadFrame) {
		t.Errorf("got %v, want ErrBadFrame", err)
	}
}

// TestRiceZigzagFolding pins the signed representation: even folded values halve, odd ones halve and
// invert. Getting the sign convention backwards decodes to plausible noise.
func TestRiceZigzagFolding(t *testing.T) {
	values := []int32{0, 1, -1, 2, -2, 100, -100, 32767, -32768}
	for _, param := range []uint{0, 1, 4, 8} {
		b := newSubframeBuilder().header(8, 0) // fixed order 0: residual is the sample
		b.rice(values, param, false)
		got := b.decode(t, len(values), 32)
		for i := range values {
			if got[i] != values[i] {
				t.Fatalf("param %d: got %v, want %v", param, got, values)
			}
		}
	}
}

// TestRice5BitParameters covers the coding method no corpus fixture uses.
func TestRice5BitParameters(t *testing.T) {
	values := []int32{1000, -2000, 30000, -30000}
	for _, param := range []uint{0, 15, 20, 30} {
		b := newSubframeBuilder().header(8, 0)
		b.rice(values, param, true)
		got := b.decode(t, len(values), 32)
		for i := range values {
			if got[i] != values[i] {
				t.Fatalf("5-bit param %d: got %v, want %v", param, got, values)
			}
		}
	}
}

// TestEscapedPartition covers the unencoded residual path, which no corpus fixture uses.
func TestEscapedPartition(t *testing.T) {
	t.Run("fixed width", func(t *testing.T) {
		values := []int32{3, -4, 1, -1}
		b := newSubframeBuilder().header(8, 0)
		b.escaped(values, 4, false)
		got := b.decode(t, len(values), 32)
		for i := range values {
			if got[i] != values[i] {
				t.Fatalf("got %v, want %v", got, values)
			}
		}
	})

	t.Run("width zero means all zero", func(t *testing.T) {
		// An escaped partition of width 0 stores nothing at all; every residual is zero.
		b := newSubframeBuilder().header(8, 0)
		b.escaped(make([]int32, 6), 0, false)
		got := b.decode(t, 6, 32)
		for i, v := range got {
			if v != 0 {
				t.Fatalf("sample %d = %d, want 0", i, v)
			}
		}
	})

	t.Run("five bit escape", func(t *testing.T) {
		values := []int32{7, -8}
		b := newSubframeBuilder().header(8, 0)
		b.escaped(values, 4, true)
		got := b.decode(t, len(values), 32)
		for i := range values {
			if got[i] != values[i] {
				t.Fatalf("got %v, want %v", got, values)
			}
		}
	})
}

// TestWastedBits covers the shift-back rule: a subframe coded without its low zero bits must have
// them restored before anything else touches the samples.
func TestWastedBits(t *testing.T) {
	// Three wasted bits: coded values are shifted left by 3 on decode.
	b := newSubframeBuilder().header(1, 3) // verbatim
	for _, v := range []int64{1, -2, 5} {
		b.signed(v, 16-3) // depth drops by the wasted count
	}
	wantSamples(t, b.decode(t, 3, 16), 8, -16, 40)
}

func TestWastedBitsRejectsOverrun(t *testing.T) {
	b := newSubframeBuilder().header(1, 16) // as many wasted bits as the depth
	_ = b.w.Write(0, 32)
	out := make([]int32, 2)
	if err := readSubframe(bitio.NewMSBReader(b.w.Bytes()), out, 16); !errors.Is(err, ErrBadFrame) {
		t.Errorf("got %v, want ErrBadFrame", err)
	}
}

func TestSubframeRejectsReservedTypes(t *testing.T) {
	for _, kind := range []uint64{2, 7, 13, 31} {
		b := newSubframeBuilder().header(kind, 0)
		_ = b.w.Write(0, 64)
		out := make([]int32, 4)
		if err := readSubframe(bitio.NewMSBReader(b.w.Bytes()), out, 16); !errors.Is(err, ErrBadFrame) {
			t.Errorf("type %d: got %v, want ErrBadFrame", kind, err)
		}
	}
}

func TestSubframeRejectsSetPadBit(t *testing.T) {
	w := bitio.NewMSBWriter()
	_ = w.Write(1, 1) // pad bit must be zero
	_ = w.Write(0, 63)
	out := make([]int32, 4)
	if err := readSubframe(bitio.NewMSBReader(w.Bytes()), out, 16); !errors.Is(err, ErrBadFrame) {
		t.Errorf("got %v, want ErrBadFrame", err)
	}
}

// TestUndecorrelate covers the three stereo modes against hand-computed values.
func TestUndecorrelate(t *testing.T) {
	t.Run("left side", func(t *testing.T) {
		ch := [][]int32{{100, -50}, {30, 10}} // left, (left-right)
		undecorrelate(ch, 2, channelsLeftSide)
		wantSamples(t, ch[0], 100, -50)
		wantSamples(t, ch[1], 70, -60)
	})

	t.Run("side right", func(t *testing.T) {
		ch := [][]int32{{30, 10}, {70, -60}} // (left-right), right
		undecorrelate(ch, 2, channelsSideRight)
		wantSamples(t, ch[0], 100, -50)
		wantSamples(t, ch[1], 70, -60)
	})

	t.Run("mid side", func(t *testing.T) {
		// left 100, right 70: mid = (100+70)>>1 = 85, side = 30.
		ch := [][]int32{{85}, {30}}
		undecorrelate(ch, 1, channelsMidSide)
		wantSamples(t, ch[0], 100)
		wantSamples(t, ch[1], 70)
	})

	t.Run("mid side odd", func(t *testing.T) {
		// left 100, right 71: mid = (100+71)>>1 = 85, side = 29. The dropped low bit of the mid
		// channel is recovered from the parity of the side channel.
		ch := [][]int32{{85}, {29}}
		undecorrelate(ch, 1, channelsMidSide)
		wantSamples(t, ch[0], 100)
		wantSamples(t, ch[1], 71)
	})

	t.Run("independent is untouched", func(t *testing.T) {
		ch := [][]int32{{1, 2}, {3, 4}}
		undecorrelate(ch, 2, channelsIndependent)
		wantSamples(t, ch[0], 1, 2)
		wantSamples(t, ch[1], 3, 4)
	})
}

// TestMidSideRoundTripExhaustive checks the reconstruction over every parity combination, since the
// odd case is where an off-by-one hides.
func TestMidSideRoundTripExhaustive(t *testing.T) {
	for left := int32(-40); left <= 40; left++ {
		for right := int32(-40); right <= 40; right++ {
			mid := (left + right) >> 1
			side := left - right
			ch := [][]int32{{mid}, {side}}
			undecorrelate(ch, 1, channelsMidSide)
			if ch[0][0] != left || ch[1][0] != right {
				t.Fatalf("left=%d right=%d round-tripped to %d and %d", left, right, ch[0][0], ch[1][0])
			}
		}
	}
}
