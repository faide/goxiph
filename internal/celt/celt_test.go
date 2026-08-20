package celt

import (
	"math/rand/v2"
	"testing"

	"github.com/faide/goxiph/internal/rangecoder"
)

// TestBandEdgesMatchTable checks the derived band table against the per-band bin counts printed in
// RFC 6716 table 55.
//
// The table gives bins per band and this holds their running totals, so a transcription slip shows
// up as a band of the wrong width rather than as anything subtler.
func TestBandEdgesMatchTable(t *testing.T) {
	// The 2.5 ms column of table 55, band by band.
	want := [NumBands]int{1, 1, 1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 6, 6, 8, 12, 18, 22}

	for b := range NumBands {
		if got := BandEdges[b+1] - BandEdges[b]; got != want[b] {
			t.Errorf("band %d has %d bins, table 55 says %d", b, got, want[b])
		}
	}
	if BandEdges[0] != 0 {
		t.Errorf("band edges start at %d, want 0", BandEdges[0])
	}
	// The bands cover 100 bins of the 120 in a 2.5 ms frame; the rest lie above 20 kHz and are not
	// coded.
	if BandEdges[NumBands] != 100 {
		t.Errorf("bands total %d bins, want 100", BandEdges[NumBands])
	}
}

// TestBandFrequenciesAreOrdered checks the frequency table for monotonicity and its endpoints.
func TestBandFrequenciesAreOrdered(t *testing.T) {
	if BandFrequencies[0] != 0 {
		t.Errorf("first band starts at %d Hz, want 0", BandFrequencies[0])
	}
	if BandFrequencies[NumBands] != 20000 {
		t.Errorf("last band ends at %d Hz, want 20000", BandFrequencies[NumBands])
	}
	for b := 1; b <= NumBands; b++ {
		if BandFrequencies[b] <= BandFrequencies[b-1] {
			t.Fatalf("band %d starts at %d Hz, not above the previous %d",
				b, BandFrequencies[b], BandFrequencies[b-1])
		}
	}
}

func TestFrameSizes(t *testing.T) {
	cases := []struct {
		f    FrameSize
		want int
	}{
		{Frame2p5ms, 120},
		{Frame5ms, 240},
		{Frame10ms, 480},
		{Frame20ms, 960},
	}
	for _, c := range cases {
		if got := c.f.Samples(); got != c.want {
			t.Errorf("frame size %d: %d samples, want %d", c.f, got, c.want)
		}
		back, err := FrameSizeForSamples(c.want)
		if err != nil {
			t.Errorf("FrameSizeForSamples(%d): %v", c.want, err)
		}
		if back != c.f {
			t.Errorf("FrameSizeForSamples(%d) = %d, want %d", c.want, back, c.f)
		}
	}
	if _, err := FrameSizeForSamples(100); err == nil {
		t.Error("accepted 100 samples as a frame length")
	}
}

// TestBinsScaleWithFrameSize pins the relation that lets one table serve all four lengths.
func TestBinsScaleWithFrameSize(t *testing.T) {
	for b := range NumBands {
		base := Frame2p5ms.Bins(b)
		for _, f := range []FrameSize{Frame5ms, Frame10ms, Frame20ms} {
			if got, want := f.Bins(b), base<<f; got != want {
				t.Fatalf("frame %d band %d: %d bins, want %d", f, b, got, want)
			}
		}
	}
	// A 20 ms frame codes 800 of its 960 bins.
	if got := Frame20ms.TotalBins(); got != 800 {
		t.Errorf("20 ms frame codes %d bins, want 800", got)
	}
}

func TestBandStartsAreContiguous(t *testing.T) {
	for _, f := range []FrameSize{Frame2p5ms, Frame5ms, Frame10ms, Frame20ms} {
		for b := range NumBands {
			if got, want := f.BandStart(b)+f.Bins(b), f.BandStart(b+1); got != want {
				t.Fatalf("frame %d: band %d ends at %d, band %d starts at %d", f, b, got, b+1, want)
			}
		}
	}
}

func TestBandForBandwidth(t *testing.T) {
	// The bandwidths stop at the frequencies RFC 6716 assigns them.
	cases := map[int]int{
		8000:  17, // wideband ends at 8 kHz
		12000: 19, // superwideband ends at 12 kHz
		20000: 21, // fullband
	}
	for freq, want := range cases {
		if got := BandForBandwidth(freq); got != want {
			t.Errorf("BandForBandwidth(%d) = %d, want %d", freq, got, want)
		}
	}
}

// TestLaplaceRoundTrip is the Laplace coder's gate.
//
// The distribution has three regimes: zero, a geometrically decaying middle, and a flat tail where
// every value shares a floor probability. A value near the boundary between two of them is where an
// off-by-one lives, so the sweep covers small magnitudes exhaustively and large ones sparsely.
func TestLaplaceRoundTrip(t *testing.T) {
	// Frequencies and decay rates in the range the energy model uses.
	models := []struct {
		fs    uint32
		decay int32
	}{
		{6000, 12000},
		{24000, 2000},
		{100, 16000},
		{30000, 100},
		{16384, 8192},
	}

	values := []int{0}
	for v := 1; v <= 40; v++ {
		values = append(values, v, -v)
	}
	values = append(values, 100, -100, 1000, -1000, 10000, -10000)

	for _, m := range models {
		enc := rangecoder.NewEncoder(1 << 16)
		coded := make([]int, len(values))
		for i, v := range values {
			coded[i] = LaplaceEncode(enc, v, m.fs, m.decay)
		}
		data := enc.Done()
		if enc.Overflowed() {
			t.Fatalf("fs=%d decay=%d: encoder overflowed", m.fs, m.decay)
		}

		dec := rangecoder.NewDecoder(data)
		for i := range values {
			got := LaplaceDecode(dec, m.fs, m.decay)
			// The encoder clamps values the model cannot express and reports what it coded, so the
			// comparison is against that rather than against the input.
			if got != coded[i] {
				t.Fatalf("fs=%d decay=%d value %d: decoded %d, encoder coded %d",
					m.fs, m.decay, values[i], got, coded[i])
			}
		}
	}
}

// TestLaplaceSmallValuesAreExact checks that the range the model is built for survives unclamped.
func TestLaplaceSmallValuesAreExact(t *testing.T) {
	const fs, decay = 6000, 12000

	for v := -20; v <= 20; v++ {
		enc := rangecoder.NewEncoder(4096)
		coded := LaplaceEncode(enc, v, fs, decay)
		if coded != v {
			t.Errorf("value %d was coded as %d", v, coded)
		}
		dec := rangecoder.NewDecoder(enc.Done())
		if got := LaplaceDecode(dec, fs, decay); got != v {
			t.Errorf("value %d round-tripped as %d", v, got)
		}
	}
}

// TestLaplaceZeroIsCheapest pins the shape of the distribution: a value further from zero must never
// cost fewer bits than one closer to it.
func TestLaplaceZeroIsCheapest(t *testing.T) {
	const fs, decay = 6000, 12000

	cost := func(v int) int {
		enc := rangecoder.NewEncoder(4096)
		LaplaceEncode(enc, v, fs, decay)
		return enc.Tell()
	}

	prev := cost(0)
	for v := 1; v <= 15; v++ {
		c := cost(v)
		if c < prev {
			t.Errorf("value %d costs %d bits, fewer than %d for value %d", v, c, prev, v-1)
		}
		prev = c
	}
}

func TestLaplaceDecodeOnEmptyInput(t *testing.T) {
	// A decoder past the end of its data supplies zeros; the result must still be a usable value.
	dec := rangecoder.NewDecoder(nil)
	for range 50 {
		v := LaplaceDecode(dec, 6000, 12000)
		if v < -100000 || v > 100000 {
			t.Fatalf("decoded %d from empty input", v)
		}
	}
}

func FuzzLaplaceRoundTrip(f *testing.F) {
	f.Add([]byte{1, 2, 3}, uint16(6000), uint16(12000))
	f.Add([]byte{0}, uint16(100), uint16(16000))

	f.Fuzz(func(t *testing.T, script []byte, fsRaw, decayRaw uint16) {
		fs := uint32(fsRaw)%32000 + 1
		decay := int32(decayRaw) % 16384
		if len(script) == 0 {
			return
		}

		values := make([]int, 0, len(script))
		for _, b := range script {
			values = append(values, int(int8(b)))
		}

		enc := rangecoder.NewEncoder(1 << 16)
		coded := make([]int, len(values))
		for i, v := range values {
			coded[i] = LaplaceEncode(enc, v, fs, decay)
		}
		data := enc.Done()
		if enc.Overflowed() {
			return
		}

		dec := rangecoder.NewDecoder(data)
		for i := range values {
			if got := LaplaceDecode(dec, fs, decay); got != coded[i] {
				t.Fatalf("value %d: decoded %d, encoder coded %d", i, got, coded[i])
			}
		}
	})
}

func BenchmarkLaplaceDecode(b *testing.B) {
	enc := rangecoder.NewEncoder(1 << 16)
	rng := rand.New(rand.NewPCG(3, 5))
	for range 10000 {
		LaplaceEncode(enc, rng.IntN(21)-10, 6000, 12000)
	}
	data := enc.Done()

	b.ReportAllocs()
	for b.Loop() {
		d := rangecoder.NewDecoder(data)
		for range 10000 {
			LaplaceDecode(d, 6000, 12000)
		}
	}
}
