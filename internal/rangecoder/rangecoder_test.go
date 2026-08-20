package rangecoder

import (
	"math/rand/v2"
	"testing"
)

// buildCDF turns frequency counts into the cumulative form both sides take.
func buildCDF(freqs []uint32) []uint32 {
	cdf := make([]uint32, len(freqs)+1)
	for i, f := range freqs {
		cdf[i+1] = cdf[i] + f
	}
	return cdf
}

// TestSymbolRoundTrip is the range coder's gate.
//
// A range coder cannot be checked a symbol at a time: its state is a single number spread across the
// whole frame, so the only meaningful question is whether a sequence written comes back identically.
func TestSymbolRoundTrip(t *testing.T) {
	cdf := buildCDF([]uint32{1, 3, 7, 20, 100, 4, 2, 60})
	rng := rand.New(rand.NewPCG(5, 11))

	for range 200 {
		count := rng.IntN(500) + 1
		want := make([]int, count)
		for i := range want {
			want[i] = rng.IntN(len(cdf) - 1)
		}

		enc := NewEncoder(4096)
		for _, k := range want {
			enc.EncodeSymbol(k, cdf)
		}
		data := enc.Done()
		if enc.Overflowed() {
			t.Fatal("encoder overflowed a 4 KiB frame")
		}

		dec := NewDecoder(data)
		for i, k := range want {
			if got := dec.DecodeSymbol(cdf); got != k {
				t.Fatalf("symbol %d of %d: got %d, want %d", i, count, got, k)
			}
		}
	}
}

// TestRangesStayInLockstep pins the property RFC 6716 section 5.1 names as the way to find a fault
// in either half: after the same symbols, both sides must hold the same range.
func TestRangesStayInLockstep(t *testing.T) {
	cdf := buildCDF([]uint32{5, 11, 2, 40, 9})
	rng := rand.New(rand.NewPCG(7, 13))

	symbols := make([]int, 300)
	for i := range symbols {
		symbols[i] = rng.IntN(len(cdf) - 1)
	}

	enc := NewEncoder(4096)
	encRanges := make([]uint32, len(symbols))
	for i, k := range symbols {
		enc.EncodeSymbol(k, cdf)
		encRanges[i] = enc.Range()
	}
	data := enc.Done()

	dec := NewDecoder(data)
	for i := range symbols {
		if got := dec.DecodeSymbol(cdf); got != symbols[i] {
			t.Fatalf("symbol %d: got %d, want %d", i, got, symbols[i])
		}
		if dec.Range() != encRanges[i] {
			t.Fatalf("symbol %d: decoder range %#x, encoder range %#x",
				i, dec.Range(), encRanges[i])
		}
	}
}

// TestRawBitsRoundTrip covers the values packed from the far end of the frame.
func TestRawBitsRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 17))

	for range 200 {
		count := rng.IntN(100) + 1
		widths := make([]uint, count)
		values := make([]uint32, count)
		for i := range count {
			widths[i] = uint(rng.IntN(24) + 1)
			values[i] = rng.Uint32() & (1<<widths[i] - 1)
		}

		enc := NewEncoder(4096)
		for i := range count {
			enc.EncodeBits(values[i], widths[i])
		}
		data := enc.Done()

		dec := NewDecoder(data)
		for i := range count {
			if got := dec.DecodeBits(widths[i]); got != values[i] {
				t.Fatalf("raw value %d (%d bits): got %#x, want %#x", i, widths[i], got, values[i])
			}
		}
	}
}

// TestSymbolsAndRawBitsInterleave covers the layout that makes this coder unusual: symbols fill the
// frame from the front while raw bits fill it from the back, and the two may overlap.
func TestSymbolsAndRawBitsInterleave(t *testing.T) {
	cdf := buildCDF([]uint32{3, 9, 1, 27})
	rng := rand.New(rand.NewPCG(19, 23))

	for range 200 {
		count := rng.IntN(200) + 1
		symbols := make([]int, count)
		raws := make([]uint32, count)
		widths := make([]uint, count)
		for i := range count {
			symbols[i] = rng.IntN(len(cdf) - 1)
			widths[i] = uint(rng.IntN(16) + 1)
			raws[i] = rng.Uint32() & (1<<widths[i] - 1)
		}

		enc := NewEncoder(8192)
		for i := range count {
			enc.EncodeSymbol(symbols[i], cdf)
			enc.EncodeBits(raws[i], widths[i])
		}
		data := enc.Done()
		if enc.Overflowed() {
			t.Fatal("encoder overflowed")
		}

		dec := NewDecoder(data)
		for i := range count {
			if got := dec.DecodeSymbol(cdf); got != symbols[i] {
				t.Fatalf("symbol %d: got %d, want %d", i, got, symbols[i])
			}
			if got := dec.DecodeBits(widths[i]); got != raws[i] {
				t.Fatalf("raw %d: got %#x, want %#x", i, got, raws[i])
			}
		}
	}
}

func TestUintRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(29, 31))

	// Values on both sides of the eight-bit split, where the coder changes strategy.
	fts := []uint32{2, 3, 16, 255, 256, 257, 1000, 65535, 65536, 1 << 20, 1<<31 - 1}

	for _, ft := range fts {
		values := []uint32{0, 1, ft / 2, ft - 1}
		for range 20 {
			values = append(values, rng.Uint32()%ft)
		}

		enc := NewEncoder(4096)
		for _, v := range values {
			enc.EncodeUint(v, ft)
		}
		data := enc.Done()
		if enc.Overflowed() {
			t.Fatalf("ft=%d: encoder overflowed", ft)
		}

		dec := NewDecoder(data)
		for i, want := range values {
			if got := dec.DecodeUint(ft); got != want {
				t.Fatalf("ft=%d value %d: got %d, want %d", ft, i, got, want)
			}
		}
	}
}

func TestBitLogpRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(37, 41))

	for _, logp := range []uint32{1, 2, 4, 8, 15} {
		bitsWanted := make([]int, 500)
		for i := range bitsWanted {
			bitsWanted[i] = rng.IntN(2)
		}

		enc := NewEncoder(4096)
		for _, b := range bitsWanted {
			enc.EncodeBitLogp(b, logp)
		}
		data := enc.Done()

		dec := NewDecoder(data)
		for i, want := range bitsWanted {
			if got := dec.DecodeBitLogp(logp); got != want {
				t.Fatalf("logp=%d bit %d: got %d, want %d", logp, i, got, want)
			}
		}
	}
}

// TestDecoderInitialisation pins the constants from RFC 6716 section 4.1.1, where an off-by-one
// desynchronises every symbol that follows.
func TestDecoderInitialisation(t *testing.T) {
	// A freshly initialised decoder reports one bit used: the bit reserved for terminating the
	// encoder.
	d := NewDecoder([]byte{0x00, 0x00, 0x00, 0x00})
	if got := d.Tell(); got != 1 {
		t.Errorf("Tell on a fresh decoder = %d, want 1", got)
	}
	if d.Range() <= codeBot {
		t.Errorf("range %#x was not normalised past %#x", d.Range(), codeBot)
	}
}

// TestReadsPastEndYieldZeros covers the rule that a decoder running off the end of its frame keeps
// supplying zeros rather than failing, which is how truncated packets stay decodable.
func TestReadsPastEndYieldZeros(t *testing.T) {
	cdf := buildCDF([]uint32{1, 1, 1, 1})

	d := NewDecoder(nil) // no bytes at all
	for range 100 {
		k := d.DecodeSymbol(cdf)
		if k < 0 || k >= len(cdf)-1 {
			t.Fatalf("symbol %d outside the alphabet", k)
		}
	}
	if got := d.DecodeBits(16); got != 0 {
		t.Errorf("raw bits past the end = %#x, want 0", got)
	}
}

func TestTellAdvances(t *testing.T) {
	cdf := buildCDF([]uint32{1, 1, 1, 1, 1, 1, 1, 1}) // three bits per symbol

	enc := NewEncoder(4096)
	for range 100 {
		enc.EncodeSymbol(3, cdf)
	}
	data := enc.Done()

	d := NewDecoder(data)
	prev := d.Tell()
	for i := range 100 {
		d.DecodeSymbol(cdf)
		now := d.Tell()
		if now < prev {
			t.Fatalf("symbol %d: Tell went backwards, %d to %d", i, prev, now)
		}
		prev = now
	}
	// Eight equiprobable symbols cost three bits each, plus the terminating bit.
	if prev < 300 || prev > 320 {
		t.Errorf("Tell after 100 three-bit symbols = %d, want about 301", prev)
	}
}

func TestTellFracIsFinerThanTell(t *testing.T) {
	cdf := buildCDF([]uint32{7, 3, 1, 5})
	enc := NewEncoder(4096)
	for range 50 {
		enc.EncodeSymbol(1, cdf)
	}
	d := NewDecoder(enc.Done())

	for range 50 {
		d.DecodeSymbol(cdf)
		whole, frac := d.Tell(), d.TellFrac()
		// TellFrac counts eighths, so it must bracket Tell.
		if frac < (whole-1)*8 || frac > whole*8+8 {
			t.Fatalf("Tell = %d, TellFrac = %d; they disagree", whole, frac)
		}
	}
}

func TestEncoderOverflowIsReported(t *testing.T) {
	cdf := buildCDF([]uint32{1, 1})
	enc := NewEncoder(4) // far too small
	for range 1000 {
		enc.EncodeSymbol(1, cdf)
	}
	enc.Done()
	if !enc.Overflowed() {
		t.Error("encoder did not report running out of room")
	}
}

func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte{1, 2, 3, 4}, uint8(4))
	f.Add([]byte{}, uint8(2))

	f.Fuzz(func(t *testing.T, script []byte, alphabet uint8) {
		n := int(alphabet%16) + 2
		freqs := make([]uint32, n)
		for i := range freqs {
			freqs[i] = uint32(i%7) + 1
		}
		cdf := buildCDF(freqs)

		symbols := make([]int, 0, len(script))
		for _, b := range script {
			symbols = append(symbols, int(b)%(n))
		}
		if len(symbols) == 0 {
			return
		}

		enc := NewEncoder(8192)
		for _, k := range symbols {
			enc.EncodeSymbol(k, cdf)
		}
		data := enc.Done()
		if enc.Overflowed() {
			return
		}

		dec := NewDecoder(data)
		for i, k := range symbols {
			if got := dec.DecodeSymbol(cdf); got != k {
				t.Fatalf("symbol %d: got %d, want %d", i, got, k)
			}
		}
	})
}

// FuzzDecodeArbitrary drives the decoder with bytes no encoder produced, which is what a corrupt or
// hostile packet looks like.
func FuzzDecodeArbitrary(f *testing.F) {
	f.Add([]byte{0xFF, 0x00, 0xAA, 0x55})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		cdf := buildCDF([]uint32{3, 1, 4, 1, 5, 9})
		d := NewDecoder(data)
		for range 200 {
			k := d.DecodeSymbol(cdf)
			if k < 0 || k >= len(cdf)-1 {
				t.Fatalf("symbol %d outside the alphabet of %d", k, len(cdf)-1)
			}
			_ = d.DecodeBits(7)
			v := d.DecodeUint(1000)
			if v >= 1000 {
				t.Fatalf("DecodeUint returned %d, outside [0, 1000)", v)
			}
		}
	})
}

func BenchmarkDecodeSymbol(b *testing.B) {
	cdf := buildCDF([]uint32{10, 20, 30, 40, 50, 60})
	enc := NewEncoder(1 << 16)
	for i := range 20000 {
		enc.EncodeSymbol(i%6, cdf)
	}
	data := enc.Done()

	b.ReportAllocs()
	for b.Loop() {
		d := NewDecoder(data)
		for range 20000 {
			d.DecodeSymbol(cdf)
		}
	}
}
