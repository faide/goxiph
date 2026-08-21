package celt

import (
	"math"
	"testing"

	"github.com/faide/goxiph/internal/rangecoder"
)

// TestIsqrt32Floors checks the integer square root against a scan, including every perfect square
// and its neighbours. The triangular angle distribution inverts a sum of consecutive integers with
// this, so landing on the wrong side of a square decodes a different angle.
func TestIsqrt32Floors(t *testing.T) {
	for v := uint32(0); v < 20000; v++ {
		if got, want := isqrt32(v), int(math.Sqrt(float64(v))); got != want {
			t.Fatalf("isqrt32(%d) = %d, want %d", v, got, want)
		}
	}
	for r := uint32(1); r < 65535; r += 97 {
		sq := r * r
		for _, tc := range []struct {
			v    uint32
			want int
		}{{sq - 1, int(r) - 1}, {sq, int(r)}, {sq + 1, int(r)}} {
			if got := isqrt32(tc.v); got != tc.want {
				t.Fatalf("isqrt32(%d) = %d, want %d", tc.v, got, tc.want)
			}
		}
	}
	// The largest input the angle decoder can produce must not overflow.
	if got := isqrt32(math.MaxUint32); got != 65535 {
		t.Errorf("isqrt32(MaxUint32) = %d, want 65535", got)
	}
}

// leafBudget finds a budget that makes the band spend exactly the given pulse count without
// splitting, and reports the pulse index. It returns false when no such budget exists.
func leafBudget(c *pulseCache, lm, band, k int) (budget, q int, ok bool) {
	run := c.run(lm, band)
	if run == nil {
		return 0, 0, false
	}
	for i := 1; i <= int(run[0]); i++ {
		if getPulses(i) != k {
			continue
		}
		budget = c.pulsesToBits(lm, band, i)
		// A budget above this threshold would split the band instead.
		if budget > int(run[run[0]])+12 {
			return 0, 0, false
		}
		return budget, i, true
	}
	return 0, 0, false
}

// TestDecodeBandLeafRoundTrips drives the leaf of the recursion end to end: a shape is encoded with
// the quantiser and read back through the band decoder, which must reproduce it normalised and
// rotated.
//
// This is the one path that can be checked without a full encoder, and it covers the join between
// the cost cache, the quantiser and the spreading rotation.
func TestDecodeBandLeafRoundTrips(t *testing.T) {
	cache := newPulseCache()
	checked := 0

	for _, band := range []int{12, 15, 17, 19, 20} {
		for _, lm := range []int{0, 1, 2, 3} {
			n := FrameSize(lm).Bins(band)
			for _, k := range []int{1, 2, 3, 5, 8} {
				budget, _, ok := leafBudget(cache, lm, band, k)
				if !ok || n <= 2 || SplitRequired(n, k) {
					continue
				}

				vec := make([]int, n)
				for i := range k {
					vec[i%n]++
				}

				enc := rangecoder.NewEncoder(1 << 12)
				EncodePVQ(enc, vec, k)
				data := enc.Done()

				want := make([]float32, n)
				Normalise(vec, want)
				Rotate(want, 1, k, SpreadNormal, false)

				got := make([]float32, n)
				bd := &bandDecoder{
					dec: rangecoder.NewDecoder(data), cache: cache,
					remaining: len(data) * 8 << BitRes, spread: SpreadNormal,
				}
				cm := bd.decodeBand(bandArgs{
					band: band, x: got, n: n, b: budget, blocks: 1,
					lm: lm, gain: 1, scratch: make([]float32, n),
				})

				if cm != 1 {
					t.Errorf("band %d LM=%d k=%d: collapse mask %b, want 1", band, lm, k, cm)
				}
				for i := range n {
					if math.Abs(float64(got[i]-want[i])) > 1e-5 {
						t.Fatalf("band %d LM=%d k=%d: coefficient %d is %v, want %v",
							band, lm, k, i, got[i], want[i])
					}
				}
				checked++
			}
		}
	}
	if checked < 10 {
		t.Fatalf("only %d leaf cases ran; the budget search is rejecting too much", checked)
	}
	t.Logf("round-tripped %d leaf bands", checked)
}

// TestDecodeBandNoiseFillIsNormalised covers the path taken when a band gets no pulses and has
// nothing to fold from. It must come out at the requested gain rather than silent, because a silent
// band inside a busy spectrum is more audible than the wrong noise.
func TestDecodeBandNoiseFillIsNormalised(t *testing.T) {
	const n = 16
	x := make([]float32, n)
	bd := &bandDecoder{
		dec: rangecoder.NewDecoder([]byte{0, 0, 0, 0}), cache: newPulseCache(),
		remaining: 0, seed: 42, spread: SpreadNormal,
	}
	cm := bd.decodeBand(bandArgs{
		band: 17, x: x, n: n, b: 0, blocks: 1, lm: 2, gain: 1,
		scratch: make([]float32, n), fill: 1,
	})

	if got := math.Sqrt(norm2(x)); math.Abs(got-1) > 1e-5 {
		t.Errorf("the filled band has norm %v, want 1", got)
	}
	if cm != 1 {
		t.Errorf("collapse mask %b, want 1", cm)
	}
	if bd.seed == 42 {
		t.Error("the seed did not advance; no noise was generated")
	}
}

// TestDecodeBandEmptyFillIsSilent is the other half: with no pulses and no block marked as holding
// anything, there is nothing to fold and the band is left at zero.
func TestDecodeBandEmptyFillIsSilent(t *testing.T) {
	const n = 16
	x := make([]float32, n)
	for i := range x {
		x[i] = 99
	}
	bd := &bandDecoder{
		dec: rangecoder.NewDecoder([]byte{0, 0, 0, 0}), cache: newPulseCache(),
		spread: SpreadNormal,
	}
	cm := bd.decodeBand(bandArgs{
		band: 17, x: x, n: n, b: 0, blocks: 1, lm: 2, gain: 1,
		scratch: make([]float32, n), fill: 0,
	})

	for i, v := range x {
		if v != 0 {
			t.Fatalf("coefficient %d is %v, want 0", i, v)
		}
	}
	if cm != 0 {
		t.Errorf("collapse mask %b, want 0", cm)
	}
}

// TestDecodeBandSingleCoefficient covers the one-sample band, which carries a sign and nothing else.
func TestDecodeBandSingleCoefficient(t *testing.T) {
	for _, want := range []float32{1, -1} {
		enc := rangecoder.NewEncoder(64)
		enc.EncodeBits(0, 1)
		if want < 0 {
			enc = rangecoder.NewEncoder(64)
			enc.EncodeBits(1, 1)
		}
		data := enc.Done()

		x := make([]float32, 1)
		out := make([]float32, 1)
		bd := &bandDecoder{
			dec: rangecoder.NewDecoder(data), cache: newPulseCache(),
			remaining: 1 << BitRes, spread: SpreadNormal,
		}
		cm := bd.decodeBand(bandArgs{
			band: 0, x: x, n: 1, b: 1 << BitRes, blocks: 1, lm: 0, gain: 1,
			lowbandOut: out, scratch: make([]float32, 1),
		})

		if x[0] != want {
			t.Errorf("decoded %v, want %v", x[0], want)
		}
		if out[0] != want {
			t.Errorf("the folding copy is %v, want %v", out[0], want)
		}
		if cm != 1 {
			t.Errorf("collapse mask %b, want 1", cm)
		}
	}
}

// TestDecodeBandSingleCoefficientWithoutBits checks the band still resolves when there is no room
// for its sign. Reading one anyway would put the decoder out of step with the encoder.
func TestDecodeBandSingleCoefficientWithoutBits(t *testing.T) {
	dec := rangecoder.NewDecoder([]byte{0xFF, 0xFF})
	before := dec.Tell()

	x := make([]float32, 1)
	bd := &bandDecoder{dec: dec, cache: newPulseCache(), remaining: 0, spread: SpreadNormal}
	bd.decodeBand(bandArgs{
		band: 0, x: x, n: 1, b: 0, blocks: 1, lm: 0, gain: 1,
		scratch: make([]float32, 1),
	})

	if x[0] != 1 {
		t.Errorf("decoded %v, want the assumed +1", x[0])
	}
	if dec.Tell() != before {
		t.Error("a sign was read with no bits available")
	}
}

// bandsFixture builds a plausible set of decoder inputs so the band loop can be driven without a
// full frame decoder.
func bandsFixture(lm int, stereo, short bool, pulsesPerBand int) *BandParams {
	return bandsFixtureTF(lm, stereo, short, pulsesPerBand, 0, false, NumBands)
}

// bandsFixtureTF adds the inputs that drive the reshaping and the stereo modes, which a fixture of
// all zeroes leaves untouched.
func bandsFixtureTF(lm int, stereo, short bool, pulsesPerBand, tf int, dual bool, intensity int) *BandParams {
	channels := 1
	if stereo {
		channels = 2
	}
	total := (1 << uint(lm)) * BandEdges[NumBands]

	p := &BandParams{
		Start: 0, End: NumBands,
		X:             make([]float32, total),
		CollapseMasks: make([]byte, NumBands*channels),
		Pulses:        make([]int, NumBands),
		TFRes:         make([]int, NumBands),
		ShortBlocks:   short,
		Spread:        SpreadNormal,
		DualStereo:    dual,
		Intensity:     intensity,
		LM:            lm,
		CodedBands:    NumBands,
	}
	if stereo {
		p.Y = make([]float32, total)
	}
	for i := range NumBands {
		p.Pulses[i] = pulsesPerBand
		p.TFRes[i] = tf
	}
	return p
}

func allFinite(t *testing.T, name string, x []float32) {
	t.Helper()
	for i, v := range x {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			t.Fatalf("%s[%d] decoded to %v", name, i, v)
		}
	}
}

// TestDecodeBandsSurvivesArbitraryInput drives the whole band loop over every combination of frame
// size, block layout and channel count, on bytes that were never encoded.
//
// A decoder reads attacker-controlled data, so the recursion has to terminate and stay in bounds on
// input that means nothing. This is the check that the split condition and the bit accounting cannot
// be driven somewhere they do not return from.
func TestDecodeBandsSurvivesArbitraryInput(t *testing.T) {
	patterns := [][]byte{
		make([]byte, 200),
		{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF},
		{0xAA, 0x55, 0xAA, 0x55, 0xAA, 0x55, 0xAA, 0x55},
		{0x01, 0x80, 0x7F, 0xC3, 0x00, 0xFE, 0x12, 0x9A, 0x33, 0x77},
	}
	for i := range patterns[0] {
		patterns[0][i] = byte(i * 37)
	}

	for _, lm := range []int{0, 1, 2, 3} {
		for _, stereo := range []bool{false, true} {
			for _, short := range []bool{false, true} {
				for _, pulses := range []int{0, 40, 200, 4000} {
					for _, data := range patterns {
						p := bandsFixture(lm, stereo, short, pulses)
						p.TotalBits = len(data) * 8 << BitRes
						DecodeBands(rangecoder.NewDecoder(data), p)

						allFinite(t, "X", p.X)
						if p.Y != nil {
							allFinite(t, "Y", p.Y)
						}
						blocks := 1
						if short {
							blocks = 1 << uint(lm)
						}
						for b, cm := range p.CollapseMasks {
							if int(cm) >= 1<<uint(blocks) {
								t.Fatalf("LM=%d short=%v: mask %d is %08b, wider than %d blocks",
									lm, short, b, cm, blocks)
							}
						}
					}
				}
			}
		}
	}
}

// TestDecodeBandsIsDeterministic checks the same bits give the same spectrum. The noise fill draws
// from a seeded generator, so a decoder that let anything else in would drift from the encoder.
func TestDecodeBandsIsDeterministic(t *testing.T) {
	data := make([]byte, 120)
	for i := range data {
		data[i] = byte(i*61 + 7)
	}

	run := func() ([]float32, []byte, uint32) {
		p := bandsFixture(3, true, false, 300)
		p.TotalBits = len(data) * 8 << BitRes
		p.Seed = 0x1234
		seed := DecodeBands(rangecoder.NewDecoder(data), p)
		return p.X, p.CollapseMasks, seed
	}

	x1, cm1, s1 := run()
	x2, cm2, s2 := run()
	for i := range x1 {
		if x1[i] != x2[i] {
			t.Fatalf("coefficient %d differs between runs: %v and %v", i, x1[i], x2[i])
		}
	}
	for i := range cm1 {
		if cm1[i] != cm2[i] {
			t.Fatalf("collapse mask %d differs between runs", i)
		}
	}
	if s1 != s2 {
		t.Errorf("the seed differs between runs: %d and %d", s1, s2)
	}
	if s1 == 0x1234 {
		t.Error("the seed never advanced; no band was noise-filled")
	}
}

// TestDecodeBandsSpendsItsBudget checks the loop consumes bits roughly in proportion to what it was
// given, and never reads past the frame. Without this the loop could return early and pass every
// structural check while decoding nothing.
func TestDecodeBandsSpendsItsBudget(t *testing.T) {
	data := make([]byte, 150)
	for i := range data {
		data[i] = byte(i*29 + 3)
	}

	totalBits := len(data) * 8 << BitRes
	var used []int
	for _, pulses := range []int{0, 100, 500, 2000} {
		p := bandsFixture(3, false, false, pulses)
		p.TotalBits = len(data) * 8 << BitRes
		dec := rangecoder.NewDecoder(data)
		DecodeBands(dec, p)

		spent := dec.TellFrac()
		if spent > p.TotalBits {
			t.Errorf("pulses=%d: read %d eighths of a %d-eighth frame", pulses, spent, p.TotalBits)
		}
		used = append(used, spent)
	}

	// The largest allocations saturate against the frame, so they are not ordered among themselves;
	// what matters is that raising the allocation from nothing does more work and then plateaus.
	if used[1] <= used[0] {
		t.Errorf("raising the allocation read no more bits: %v", used)
	}
	if used[len(used)-1] < used[0] {
		t.Errorf("the largest allocation read less than the smallest: %v", used)
	}
	if used[len(used)-1]*10 < totalBits*9 {
		t.Errorf("the largest allocation left the frame mostly unread: %v of %d", used, totalBits)
	}
	t.Logf("bits read at rising allocations: %v of %d", used, len(data)*8<<BitRes)
}

func FuzzDecodeBands(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7}, 3, true, false, 500)
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF}, 0, false, true, 0)
	f.Add(make([]byte, 64), 2, true, true, 3000)

	f.Fuzz(func(t *testing.T, data []byte, lm int, stereo, short bool, pulses int) {
		if len(data) == 0 || len(data) > 2000 {
			return
		}
		lm &= 3
		if pulses < 0 {
			pulses = -pulses
		}
		pulses %= 20000

		p := bandsFixture(lm, stereo, short, pulses)
		p.TotalBits = len(data) * 8 << BitRes
		DecodeBands(rangecoder.NewDecoder(data), p)

		allFinite(t, "X", p.X)
		if p.Y != nil {
			allFinite(t, "Y", p.Y)
		}
	})
}

// TestDecodeBandsWithReshaping drives the time-frequency changes, which a fixture of all zeroes
// never reaches: a positive change merges blocks for frequency resolution and a negative one splits
// them for time resolution, and each has an undo on the way out.
//
// The changes come from tfSelectTable rather than being invented, because the two halves of the
// setting are not independent. Only a transient frame can merge blocks, and only a transient frame
// runs on more than one; feeding a merge to a long-block frame asks it to halve a single block.
func TestDecodeBandsWithReshaping(t *testing.T) {
	data := make([]byte, 180)
	for i := range data {
		data[i] = byte(i*53 + 11)
	}

	seen := map[int]bool{}
	for _, lm := range []int{0, 1, 2, 3} {
		for _, transient := range []bool{false, true} {
			base := 0
			if transient {
				base = 4
			}
			for entry := range 4 {
				tf := int(tfSelectTable[lm][base+entry])
				seen[tf] = true
				for _, stereo := range []bool{false, true} {
					p := bandsFixtureTF(lm, stereo, transient, 600, tf, false, NumBands)
					p.TotalBits = len(data) * 8 << BitRes
					DecodeBands(rangecoder.NewDecoder(data), p)

					allFinite(t, "X", p.X)
					if p.Y != nil {
						allFinite(t, "Y", p.Y)
					}
				}
			}
		}
	}
	for _, want := range []int{-3, -2, -1, 0, 1, 2, 3} {
		if !seen[want] {
			t.Errorf("a change of %d was never exercised", want)
		}
	}
}

// TestDecodeBandsStereoModes covers dual stereo and the switch to intensity stereo partway up the
// spectrum, where the two running spectra merge into one.
func TestDecodeBandsStereoModes(t *testing.T) {
	data := make([]byte, 160)
	for i := range data {
		data[i] = byte(i*71 + 5)
	}

	for _, dual := range []bool{false, true} {
		for _, intensity := range []int{0, 5, 12, NumBands} {
			for _, lm := range []int{0, 3} {
				for _, tf := range []int{-3, -2, -1, 0} {
					p := bandsFixtureTF(lm, true, false, 700, tf, dual, intensity)
					p.TotalBits = len(data) * 8 << BitRes
					DecodeBands(rangecoder.NewDecoder(data), p)

					allFinite(t, "X", p.X)
					allFinite(t, "Y", p.Y)
				}
			}
		}
	}
}

// TestStereoMergeHandlesACollapsedChannel covers the guard for a mid and side that cancel: with one
// side of the pair at nothing there is no norm to scale by, and both channels take the mid.
func TestStereoMergeHandlesACollapsedChannel(t *testing.T) {
	// A side equal to the scaled mid makes one of the two sums vanish.
	x := []float32{1, 0, 0, 0}
	y := []float32{1, 0, 0, 0}
	stereoMerge(x, y, 1)

	allFinite(t, "X", x)
	allFinite(t, "Y", y)
	for i := range x {
		if x[i] != y[i] {
			t.Fatalf("coefficient %d: %v and %v differ; the collapse guard did not copy", i, x[i], y[i])
		}
	}

	// A healthy pair must still be separated, or the guard is firing everywhere.
	x2 := []float32{1, 0, 0, 0}
	y2 := []float32{0, 1, 0, 0}
	stereoMerge(x2, y2, 0.7)
	same := true
	for i := range x2 {
		if x2[i] != y2[i] {
			same = false
		}
	}
	if same {
		t.Error("an orthogonal mid and side came out identical")
	}
}
