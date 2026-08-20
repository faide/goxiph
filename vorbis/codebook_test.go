package vorbis

import (
	"errors"
	"math"
	"testing"

	"github.com/faide/goxiph/internal/bitio"
)

// TestCodewordAssignmentMatchesSpecExample is the decisive test for the Huffman construction.
//
// Vorbis I 3.2.1 works this exact list through and gives the expected codewords. Assignment is in
// entry order rather than sorted by length, so a textbook canonical code fails here: it would give
// entry 5 the codeword 01 instead of 10.
func TestCodewordAssignmentMatchesSpecExample(t *testing.T) {
	lengths := []uint8{2, 4, 4, 4, 4, 2, 3, 3}
	want := []struct {
		length int
		code   uint32
	}{
		{2, 0b00},
		{4, 0b0100},
		{4, 0b0101},
		{4, 0b0110},
		{4, 0b0111},
		{2, 0b10},
		{3, 0b110},
		{3, 0b111},
	}

	c := &Codebook{Entries: len(lengths), lengths: lengths, single: -1}
	if err := c.buildTree(); err != nil {
		t.Fatalf("buildTree: %v", err)
	}
	for i, w := range want {
		if got := c.Codeword(i); got != w.code {
			t.Errorf("entry %d: codeword %0*b, want %0*b", i, w.length, got, w.length, w.code)
		}
		if got := c.CodewordLength(i); got != w.length {
			t.Errorf("entry %d: length %d, want %d", i, got, w.length)
		}
	}
}

// TestDecodeRoundTripSpecExample feeds each codeword back through the decoder, bit by bit in the
// order the bitpacker would transmit it.
func TestDecodeRoundTripSpecExample(t *testing.T) {
	lengths := []uint8{2, 4, 4, 4, 4, 2, 3, 3}
	c := &Codebook{Entries: len(lengths), lengths: lengths, single: -1}
	if err := c.buildTree(); err != nil {
		t.Fatalf("buildTree: %v", err)
	}

	for entry := range lengths {
		// A codeword is transmitted most significant bit first, so write it bit by bit.
		w := bitio.NewLSBWriter()
		code, n := c.Codeword(entry), c.CodewordLength(entry)
		for b := n - 1; b >= 0; b-- {
			_ = w.WriteBit((code >> b) & 1)
		}
		// Pad so the reader has a whole byte available.
		_ = w.Write(0, 8)

		got, err := c.DecodeScalar(bitio.NewLSBReader(w.Bytes()))
		if err != nil {
			t.Fatalf("entry %d: DecodeScalar: %v", entry, err)
		}
		if got != entry {
			t.Errorf("entry %d decoded as %d", entry, got)
		}
	}
}

func TestUnderpopulatedTreeRejected(t *testing.T) {
	// The spec's example with entry 7 removed leaves the tree unfinished.
	lengths := []uint8{2, 4, 4, 4, 4, 2, 3}
	c := &Codebook{Entries: len(lengths), lengths: lengths, single: -1}
	if err := c.buildTree(); !errors.Is(err, ErrBadCodebook) {
		t.Errorf("got %v, want ErrBadCodebook for an underpopulated tree", err)
	}
}

func TestOverpopulatedTreeRejected(t *testing.T) {
	// A ninth codeword cannot fit alongside the spec's fully populated example.
	lengths := []uint8{2, 4, 4, 4, 4, 2, 3, 3, 3}
	c := &Codebook{Entries: len(lengths), lengths: lengths, single: -1}
	if err := c.buildTree(); !errors.Is(err, ErrBadCodebook) {
		t.Errorf("got %v, want ErrBadCodebook for an overpopulated tree", err)
	}
}

// TestSingleEntryCodebook covers errata 20150226: a book with one used entry is underpopulated and
// therefore malformed, but streams using it exist and must decode.
func TestSingleEntryCodebook(t *testing.T) {
	t.Run("length 1 accepted", func(t *testing.T) {
		c := &Codebook{Entries: 3, lengths: []uint8{0, 1, 0}, single: -1}
		if err := c.buildTree(); err != nil {
			t.Fatalf("buildTree: %v", err)
		}
		// Reading sinks one bit and returns the only entry, whichever value the bit has.
		for _, data := range [][]byte{{0x00}, {0xFF}} {
			r := bitio.NewLSBReader(data)
			got, err := c.DecodeScalar(r)
			if err != nil {
				t.Fatalf("DecodeScalar: %v", err)
			}
			if got != 1 {
				t.Errorf("got entry %d, want 1", got)
			}
			if r.BitsRead() != 1 {
				t.Errorf("consumed %d bits, want 1", r.BitsRead())
			}
		}
	})

	t.Run("other lengths rejected", func(t *testing.T) {
		for _, length := range []uint8{2, 5, 31} {
			c := &Codebook{Entries: 3, lengths: []uint8{0, length, 0}, single: -1}
			if err := c.buildTree(); !errors.Is(err, ErrBadCodebook) {
				t.Errorf("length %d: got %v, want ErrBadCodebook", length, err)
			}
		}
	})
}

func TestSparseCodebookSkipsUnusedEntries(t *testing.T) {
	// Entries 1 and 3 are unused; the rest form a complete tree.
	lengths := []uint8{1, 0, 2, 0, 2}
	c := &Codebook{Entries: len(lengths), lengths: lengths, single: -1}
	if err := c.buildTree(); err != nil {
		t.Fatalf("buildTree: %v", err)
	}
	for _, unused := range []int{1, 3} {
		if c.CodewordLength(unused) != 0 {
			t.Errorf("entry %d should carry no codeword", unused)
		}
	}
	// No bit pattern may decode to an unused entry.
	for _, data := range [][]byte{{0x00}, {0x01}, {0x02}, {0x03}, {0xFF}} {
		got, err := c.DecodeScalar(bitio.NewLSBReader(data))
		if err != nil {
			continue
		}
		if got == 1 || got == 3 {
			t.Errorf("data %#02x decoded to unused entry %d", data[0], got)
		}
	}
}

func TestIlog(t *testing.T) {
	cases := map[int32]uint{0: 0, 1: 1, 2: 2, 3: 2, 4: 3, 7: 3, 8: 4, -1: 0, -100: 0}
	for x, want := range cases {
		if got := ilog(x); got != want {
			t.Errorf("ilog(%d) = %d, want %d", x, got, want)
		}
	}
}

// TestFloat32Unpack checks the Vorbis packed float format, which is not IEEE 754.
func TestFloat32Unpack(t *testing.T) {
	// mantissa 1, exponent 788 gives 1 * 2^0.
	if got := float32Unpack(1 | (788 << 21)); got != 1 {
		t.Errorf("got %v, want 1", got)
	}
	// The sign bit negates the mantissa rather than following IEEE layout.
	if got := float32Unpack(1 | (788 << 21) | 0x80000000); got != -1 {
		t.Errorf("got %v, want -1", got)
	}
	if got := float32Unpack(0); got != 0 {
		t.Errorf("got %v, want 0", got)
	}
	// mantissa 3, exponent 789 gives 3 * 2^1.
	if got := float32Unpack(3 | (789 << 21)); got != 6 {
		t.Errorf("got %v, want 6", got)
	}
	// A large mantissa with a small exponent stays finite.
	if got := float32Unpack(0x1fffff); math.IsInf(float64(got), 0) || math.IsNaN(float64(got)) {
		t.Errorf("got %v, want a finite value", got)
	}
}

func TestLookup1Values(t *testing.T) {
	cases := []struct {
		entries, dims, want int
	}{
		{1, 1, 1},
		{16, 2, 4}, // 4^2 == 16
		{17, 2, 4}, // 5^2 == 25 > 17
		{27, 3, 3}, // 3^3 == 27
		{26, 3, 2}, // 3^3 == 27 > 26
		{1000, 1, 1000},
		{0, 2, 0},
		{100, 0, 0},
	}
	for _, c := range cases {
		if got := lookup1Values(c.entries, c.dims); got != c.want {
			t.Errorf("lookup1Values(%d, %d) = %d, want %d", c.entries, c.dims, got, c.want)
		}
	}
}

// TestLookup1ValuesBoundary pins that the result is exact at the power boundary, where computing it
// from a logarithm lands on the wrong side.
func TestLookup1ValuesBoundary(t *testing.T) {
	for n := 2; n <= 40; n++ {
		for dims := 2; dims <= 4; dims++ {
			exact := 1
			for range dims {
				exact *= n
			}
			if got := lookup1Values(exact, dims); got != n {
				t.Errorf("lookup1Values(%d^%d = %d, %d) = %d, want %d", n, dims, exact, dims, got, n)
			}
			if got := lookup1Values(exact-1, dims); got != n-1 {
				t.Errorf("lookup1Values(%d, %d) = %d, want %d", exact-1, dims, got, n-1)
			}
		}
	}
}

// packCodebook builds a codebook packet for the round-trip tests.
func packCodebook(t *testing.T, dims, entries int, lengths []uint8, lookupType int, valueBits uint, mult []uint32) []byte {
	t.Helper()
	w := bitio.NewLSBWriter()
	_ = w.Write(codebookSync, 24)
	_ = w.Write(uint32(dims), 16)
	_ = w.Write(uint32(entries), 24)
	_ = w.Write(0, 1) // not ordered
	_ = w.Write(1, 1) // sparse
	for _, l := range lengths {
		if l == 0 {
			_ = w.Write(0, 1)
			continue
		}
		_ = w.Write(1, 1)
		_ = w.Write(uint32(l-1), 5)
	}
	_ = w.Write(uint32(lookupType), 4)
	if lookupType != 0 {
		_ = w.Write(1|(788<<21), 32) // minimum 1.0
		_ = w.Write(1|(788<<21), 32) // delta 1.0
		_ = w.Write(uint32(valueBits-1), 4)
		_ = w.Write(0, 1) // sequence_p clear
		for _, m := range mult {
			_ = w.Write(m, valueBits)
		}
	}
	_ = w.Write(0, 8) // padding so reads near the end succeed
	return w.Bytes()
}

func TestReadCodebookRoundTrip(t *testing.T) {
	lengths := []uint8{2, 4, 4, 4, 4, 2, 3, 3}
	packet := packCodebook(t, 1, len(lengths), lengths, 0, 0, nil)

	c, err := readCodebook(bitio.NewLSBReader(packet))
	if err != nil {
		t.Fatalf("readCodebook: %v", err)
	}
	if c.Dimensions != 1 || c.Entries != 8 || c.LookupType != 0 {
		t.Errorf("got dims=%d entries=%d lookup=%d", c.Dimensions, c.Entries, c.LookupType)
	}
	for i, want := range lengths {
		if got := c.CodewordLength(i); got != int(want) {
			t.Errorf("entry %d: length %d, want %d", i, got, want)
		}
	}
}

func TestReadCodebookRejects(t *testing.T) {
	lengths := []uint8{1, 1}
	good := packCodebook(t, 1, 2, lengths, 0, 0, nil)

	t.Run("bad sync", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[0] ^= 0xff
		if _, err := readCodebook(bitio.NewLSBReader(bad)); !errors.Is(err, ErrBadCodebook) {
			t.Errorf("got %v, want ErrBadCodebook", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		for n := range len(good) - 1 {
			if _, err := readCodebook(bitio.NewLSBReader(good[:n])); err == nil {
				t.Errorf("accepted a codebook truncated to %d bytes", n)
			}
		}
	})
	t.Run("reserved lookup type", func(t *testing.T) {
		bad := packCodebook(t, 1, 2, lengths, 3, 0, nil)
		if _, err := readCodebook(bitio.NewLSBReader(bad)); !errors.Is(err, ErrBadCodebook) {
			t.Errorf("got %v, want ErrBadCodebook", err)
		}
	})
}

// TestEntryCountIsNotTrusted covers the allocation guard: a 24-bit entry count from an untrusted
// stream must be checked against the bits available.
func TestEntryCountIsNotTrusted(t *testing.T) {
	w := bitio.NewLSBWriter()
	_ = w.Write(codebookSync, 24)
	_ = w.Write(1, 16)
	_ = w.Write(0xFFFFFF, 24) // 16.7 million entries in a 7-byte packet
	_ = w.Write(0, 8)

	if _, err := readCodebook(bitio.NewLSBReader(w.Bytes())); !errors.Is(err, ErrBadCodebook) {
		t.Errorf("got %v, want ErrBadCodebook", err)
	}
}

func TestEntryVectorLookupType2(t *testing.T) {
	// Two entries of two dimensions, multiplicands 1..4, delta 1, minimum 1, sequence_p clear.
	lengths := []uint8{1, 1}
	packet := packCodebook(t, 2, 2, lengths, 2, 4, []uint32{1, 2, 3, 4})

	c, err := readCodebook(bitio.NewLSBReader(packet))
	if err != nil {
		t.Fatalf("readCodebook: %v", err)
	}
	if c.LookupValues != 4 {
		t.Fatalf("LookupValues = %d, want 4", c.LookupValues)
	}

	dst := make([]float32, 2)
	if err := c.EntryVector(0, dst); err != nil {
		t.Fatalf("EntryVector: %v", err)
	}
	// value = multiplicand*delta + minimum = m*1 + 1
	if dst[0] != 2 || dst[1] != 3 {
		t.Errorf("entry 0 vector = %v, want [2 3]", dst)
	}
	if err := c.EntryVector(1, dst); err != nil {
		t.Fatalf("EntryVector: %v", err)
	}
	if dst[0] != 4 || dst[1] != 5 {
		t.Errorf("entry 1 vector = %v, want [4 5]", dst)
	}
}

func TestEntryVectorRejectsLookupType0(t *testing.T) {
	packet := packCodebook(t, 1, 2, []uint8{1, 1}, 0, 0, nil)
	c, err := readCodebook(bitio.NewLSBReader(packet))
	if err != nil {
		t.Fatalf("readCodebook: %v", err)
	}
	if err := c.EntryVector(0, make([]float32, 1)); !errors.Is(err, ErrBadCodebook) {
		t.Errorf("got %v, want ErrBadCodebook", err)
	}
}

func FuzzReadCodebook(f *testing.F) {
	f.Add(packCodebookRaw([]uint8{2, 4, 4, 4, 4, 2, 3, 3}))
	f.Add(packCodebookRaw([]uint8{1, 1}))
	f.Add([]byte{0x42, 0x43, 0x56})

	f.Fuzz(func(t *testing.T, data []byte) {
		c, err := readCodebook(bitio.NewLSBReader(data))
		if err != nil {
			return
		}
		// A book that parsed must decode without panicking, whatever bits follow.
		r := bitio.NewLSBReader(data)
		for range 32 {
			entry, err := c.DecodeScalar(r)
			if err != nil {
				break
			}
			if entry < 0 || entry >= c.Entries {
				t.Fatalf("decoded entry %d outside 0..%d", entry, c.Entries-1)
			}
			if c.LookupType != 0 {
				dst := make([]float32, c.Dimensions)
				if err := c.EntryVector(entry, dst); err != nil {
					t.Fatalf("EntryVector on a parsed book: %v", err)
				}
			}
		}
	})
}

// packCodebookRaw mirrors packCodebook without a *testing.T, for fuzz seeds.
func packCodebookRaw(lengths []uint8) []byte {
	w := bitio.NewLSBWriter()
	_ = w.Write(codebookSync, 24)
	_ = w.Write(1, 16)
	_ = w.Write(uint32(len(lengths)), 24)
	_ = w.Write(0, 1)
	_ = w.Write(1, 1)
	for _, l := range lengths {
		if l == 0 {
			_ = w.Write(0, 1)
			continue
		}
		_ = w.Write(1, 1)
		_ = w.Write(uint32(l-1), 5)
	}
	_ = w.Write(0, 4)
	_ = w.Write(0, 8)
	return w.Bytes()
}

func BenchmarkDecodeScalar(b *testing.B) {
	lengths := []uint8{2, 4, 4, 4, 4, 2, 3, 3}
	c := &Codebook{Entries: len(lengths), lengths: lengths, single: -1}
	if err := c.buildTree(); err != nil {
		b.Fatal(err)
	}
	data := make([]byte, 4096)
	for i := range data {
		data[i] = byte(i * 31)
	}
	r := bitio.NewLSBReader(data)

	b.ReportAllocs()
	for b.Loop() {
		r.Reset(data)
		for {
			if _, err := c.DecodeScalar(r); err != nil {
				break
			}
		}
	}
}

func FuzzParseSetup(f *testing.F) {
	f.Add([]byte{packetSetup, 'v', 'o', 'r', 'b', 'i', 's'})
	f.Add(append([]byte{packetSetup}, "vorbis\x00"...))

	info := Info{Channels: 2, SampleRate: 44100, BlockSize0: 256, BlockSize1: 2048}
	f.Fuzz(func(t *testing.T, data []byte) {
		s, err := ParseSetup(data, info)
		if err != nil {
			return
		}
		// Anything that parsed must be internally consistent, since the decoder will trust it.
		for i, m := range s.Modes {
			if m.Mapping < 0 || m.Mapping >= len(s.Mappings) {
				t.Fatalf("mode %d references mapping %d of %d", i, m.Mapping, len(s.Mappings))
			}
		}
		for i, m := range s.Mappings {
			for j, fl := range m.SubmapFloor {
				if fl < 0 || fl >= len(s.Floors) {
					t.Fatalf("mapping %d submap %d references floor %d of %d", i, j, fl, len(s.Floors))
				}
			}
			for j, re := range m.SubmapResidue {
				if re < 0 || re >= len(s.Residues) {
					t.Fatalf("mapping %d submap %d references residue %d of %d", i, j, re, len(s.Residues))
				}
			}
			for j, mux := range m.Mux {
				if mux < 0 || mux >= len(m.SubmapFloor) {
					t.Fatalf("mapping %d channel %d uses submap %d of %d", i, j, mux, len(m.SubmapFloor))
				}
			}
		}
		for i, res := range s.Residues {
			if res.Classbook < 0 || res.Classbook >= len(s.Codebooks) {
				t.Fatalf("residue %d classbook %d of %d", i, res.Classbook, len(s.Codebooks))
			}
		}
	})
}
