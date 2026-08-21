package celt

import (
	"math"
	"testing"

	"github.com/faide/goxiph/internal/rangecoder"
)

// encodeCoarse mirrors the decoder so a sequence of prediction errors can be planted and read back.
//
// It writes the values it is given rather than searching for them, which is what makes it useful for
// a test: the input is known exactly.
func encodeCoarse(e *rangecoder.Encoder, qis [][]int, start, end int, frame FrameSize, intra bool) {
	model := &energyProbModel[frame][boolToInt(intra)]
	for i := start; i < end; i++ {
		for c := range qis {
			pi := 2 * min(i, 20)
			LaplaceEncode(e, qis[c][i], uint32(model[pi])<<7, int32(model[pi+1])<<6)
		}
	}
}

// TestEnergyProbModelShape checks the table's dimensions and its endpoints.
//
// The table was generated from the reference rather than typed, so what matters is that the
// generation landed the right values in the right places; TestConformanceEnergyModelMatchesReference
// checks it against the source itself.
func TestEnergyProbModelShape(t *testing.T) {
	// The first pair of the 120-sample inter model, and the last pair of the 960-sample intra one.
	if got := energyProbModel[Frame2p5ms][0][0]; got != 72 {
		t.Errorf("first inter probability = %d, want 72", got)
	}
	if got := energyProbModel[Frame2p5ms][0][1]; got != 127 {
		t.Errorf("first inter decay = %d, want 127", got)
	}
	if got := energyProbModel[Frame20ms][1][41]; got != 40 {
		t.Errorf("last intra decay = %d, want 40", got)
	}

	// Every entry must be non-zero in the probability slot, or a band would be impossible to code.
	for lm := range 4 {
		for intra := range 2 {
			for b := range 21 {
				if energyProbModel[lm][intra][2*b] == 0 {
					t.Errorf("frame %d intra=%d band %d has zero probability", lm, intra, b)
				}
			}
		}
	}
}

// TestCoarseEnergyPredictionArithmetic checks the prediction filter against values worked out by
// hand, rather than against another implementation of the same formula.
//
// With intra prediction the time coefficient is zero, so each band's energy is the running frequency
// prediction plus this band's error, and the running term advances by the error scaled by one minus
// beta. That makes the whole chain checkable on paper.
func TestCoarseEnergyPredictionArithmetic(t *testing.T) {
	const frame = Frame20ms
	qis := [][]int{{1, 2, -1}}

	enc := rangecoder.NewEncoder(4096)
	encodeCoarse(enc, qis, 0, 3, frame, true)
	data := enc.Done()

	// Start from a non-zero previous frame to prove the time coefficient is ignored when intra.
	energy := [][]float32{{5, 5, 5}}
	dec := rangecoder.NewDecoder(data)
	DecodeCoarseEnergy(dec, energy, 0, 3, frame, true, len(data)*8)

	// prev advances by qi*(1-betaIntra); energy is prev-before-update plus qi.
	k := float64(1 - betaIntra)
	want := []float64{
		0 + 1,
		1*k + 2,
		1*k + 2*k + (-1),
	}
	for i, w := range want {
		if math.Abs(float64(energy[0][i])-w) > 1e-5 {
			t.Errorf("band %d energy = %v, want %v", i, energy[0][i], w)
		}
	}
}

// TestCoarseEnergyUsesTimePrediction checks that a non-intra frame reads the previous frame's values,
// which is the difference intra mode exists to remove.
func TestCoarseEnergyUsesTimePrediction(t *testing.T) {
	const frame = Frame20ms
	qis := [][]int{{0, 0, 0}}

	enc := rangecoder.NewEncoder(4096)
	encodeCoarse(enc, qis, 0, 3, frame, false)
	data := enc.Done()

	energy := [][]float32{{4, 4, 4}}
	dec := rangecoder.NewDecoder(data)
	DecodeCoarseEnergy(dec, energy, 0, 3, frame, false, len(data)*8)

	// With zero prediction error the result is the previous frame scaled by the time coefficient.
	want := float64(4 * predCoef[frame])
	if math.Abs(float64(energy[0][0])-want) > 1e-5 {
		t.Errorf("band 0 energy = %v, want %v", energy[0][0], want)
	}
	if energy[0][0] == 4 {
		t.Error("energy passed through unchanged; time prediction was not applied")
	}
}

// TestCoarseEnergyFloorsThePreviousFrame covers the clamp that keeps a fixed-point decoder in the
// same state as a floating-point one.
func TestCoarseEnergyFloorsThePreviousFrame(t *testing.T) {
	const frame = Frame20ms
	qis := [][]int{{0}}

	enc := rangecoder.NewEncoder(4096)
	encodeCoarse(enc, qis, 0, 1, frame, false)
	data := enc.Done()

	// A previous value far below the floor must be treated as the floor.
	energy := [][]float32{{-1000}}
	dec := rangecoder.NewDecoder(data)
	DecodeCoarseEnergy(dec, energy, 0, 1, frame, false, len(data)*8)

	want := float64(minCoarseEnergy * predCoef[frame])
	if math.Abs(float64(energy[0][0])-want) > 1e-4 {
		t.Errorf("energy = %v, want %v (the floor scaled by the coefficient)", energy[0][0], want)
	}
}

// TestCoarseEnergyRoundTrip checks that a range of prediction errors survives across frame sizes and
// both prediction modes.
func TestCoarseEnergyRoundTrip(t *testing.T) {
	for _, frame := range []FrameSize{Frame2p5ms, Frame5ms, Frame10ms, Frame20ms} {
		for _, intra := range []bool{false, true} {
			qis := [][]int{make([]int, NumBands), make([]int, NumBands)}
			for c := range qis {
				for i := range NumBands {
					qis[c][i] = (i*7+c*3)%9 - 4
				}
			}

			enc := rangecoder.NewEncoder(1 << 14)
			encodeCoarse(enc, qis, 0, NumBands, frame, intra)
			data := enc.Done()

			// Decode twice from the same start, once through the package and once by repeating the
			// filter here, and require the two to agree.
			energy := [][]float32{make([]float32, NumBands), make([]float32, NumBands)}
			dec := rangecoder.NewDecoder(data)
			DecodeCoarseEnergy(dec, energy, 0, NumBands, frame, intra, len(data)*8)

			var coef, beta float32
			if intra {
				coef, beta = 0, betaIntra
			} else {
				coef, beta = predCoef[frame], betaCoef[frame]
			}
			var prev [2]float32
			for i := range NumBands {
				for c := range 2 {
					want := coef*0 + prev[c] + float32(qis[c][i])
					if math.Abs(float64(energy[c][i]-want)) > 1e-4 {
						t.Fatalf("frame %d intra=%v channel %d band %d: %v, want %v",
							frame, intra, c, i, energy[c][i], want)
					}
					prev[c] += float32(qis[c][i]) - beta*float32(qis[c][i])
				}
			}
			_ = coef
		}
	}
}

// TestCoarseEnergyDegradesWhenBitsRunOut covers the fallbacks of RFC 6716 section 4.3.2.1.
//
// A frame with too few bits left cannot use the Laplace model, and the decoder steps down through a
// three-symbol context, a single bit, and finally an assumed value. Without this a short frame would
// desynchronise instead of degrading.
func TestCoarseEnergyDegradesWhenBitsRunOut(t *testing.T) {
	for _, budget := range []int{0, 1, 2, 8, 14, 15, 100} {
		energy := [][]float32{make([]float32, NumBands)}
		dec := rangecoder.NewDecoder([]byte{0x55, 0xAA, 0x33})
		// Must return rather than spin or panic, whatever the budget.
		DecodeCoarseEnergy(dec, energy, 0, NumBands, Frame20ms, true, budget)

		for i, v := range energy[0] {
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				t.Fatalf("budget %d band %d decoded to %v", budget, i, v)
			}
		}
	}
}

// TestFineEnergyRefinement checks the mapping from a fine value to its correction.
//
// The refinement is centred on zero: the lowest code moves the energy down by just under half a
// step and the highest up by just under half, so an unrefined band is not biased in either
// direction.
func TestFineEnergyRefinement(t *testing.T) {
	const bits = 3
	fineBits := make([]int, NumBands)
	fineBits[0] = bits

	for q := range 1 << bits {
		enc := rangecoder.NewEncoder(64)
		enc.EncodeBits(uint32(q), bits)
		data := enc.Done()

		energy := [][]float32{make([]float32, NumBands)}
		dec := rangecoder.NewDecoder(data)
		DecodeFineEnergy(dec, energy, 0, 1, fineBits)

		want := (float64(q)+0.5)/float64(int(1)<<bits) - 0.5
		if math.Abs(float64(energy[0][0])-want) > 1e-6 {
			t.Errorf("q=%d: offset %v, want %v", q, energy[0][0], want)
		}
	}

	// The corrections must straddle zero and stay inside half a step.
	enc := rangecoder.NewEncoder(64)
	enc.EncodeBits(0, bits)
	dec := rangecoder.NewDecoder(enc.Done())
	energy := [][]float32{make([]float32, NumBands)}
	DecodeFineEnergy(dec, energy, 0, 1, fineBits)
	if energy[0][0] >= 0 {
		t.Errorf("the lowest fine code gave %v, want a negative correction", energy[0][0])
	}
}

func TestFineEnergySkipsUnallocatedBands(t *testing.T) {
	fineBits := make([]int, NumBands) // every band zero
	energy := [][]float32{make([]float32, NumBands)}

	dec := rangecoder.NewDecoder([]byte{0xFF, 0xFF})
	DecodeFineEnergy(dec, energy, 0, NumBands, fineBits)

	for i, v := range energy[0] {
		if v != 0 {
			t.Errorf("band %d moved to %v with no bits allocated", i, v)
		}
	}
}

// TestFinalEnergyRespectsPriority covers the two-pass spend of RFC 6716 section 4.3.2.2: bands of
// priority zero all receive a bit before any band of priority one does.
func TestFinalEnergyRespectsPriority(t *testing.T) {
	fineBits := make([]int, NumBands)
	priority := make([]int, NumBands)
	// Band 0 is low priority, band 1 high, so band 1 must be served first.
	priority[0] = 1
	priority[1] = 0

	enc := rangecoder.NewEncoder(64)
	enc.EncodeBits(1, 1) // the single bit available
	data := enc.Done()

	energy := [][]float32{make([]float32, NumBands)}
	dec := rangecoder.NewDecoder(data)
	left := DecodeFinalEnergy(dec, energy, 0, NumBands, fineBits, priority, 1)

	if energy[0][1] == 0 {
		t.Error("the priority 0 band received no refinement")
	}
	if energy[0][0] != 0 {
		t.Error("the priority 1 band was served before priority 0 ran out")
	}
	if left != 0 {
		t.Errorf("%d bits left, want 0", left)
	}
}

func TestFinalEnergySkipsSaturatedBands(t *testing.T) {
	fineBits := make([]int, NumBands)
	priority := make([]int, NumBands)
	for i := range fineBits {
		fineBits[i] = MaxFineBits // every band already at the limit
	}

	energy := [][]float32{make([]float32, NumBands)}
	dec := rangecoder.NewDecoder([]byte{0xFF, 0xFF})
	left := DecodeFinalEnergy(dec, energy, 0, NumBands, fineBits, priority, 16)

	if left != 16 {
		t.Errorf("%d bits left, want all 16 unspent", left)
	}
	for i, v := range energy[0] {
		if v != 0 {
			t.Errorf("band %d moved to %v though it was saturated", i, v)
		}
	}
}

func BenchmarkDecodeCoarseEnergy(b *testing.B) {
	qis := [][]int{make([]int, NumBands), make([]int, NumBands)}
	for c := range qis {
		for i := range NumBands {
			qis[c][i] = i%7 - 3
		}
	}
	enc := rangecoder.NewEncoder(1 << 14)
	encodeCoarse(enc, qis, 0, NumBands, Frame20ms, false)
	data := enc.Done()

	energy := [][]float32{make([]float32, NumBands), make([]float32, NumBands)}
	b.ReportAllocs()
	for b.Loop() {
		dec := rangecoder.NewDecoder(data)
		DecodeCoarseEnergy(dec, energy, 0, NumBands, Frame20ms, false, len(data)*8)
	}
}
