package vorbis

import (
	"testing"

	"github.com/faide/goxiph/internal/bitio"
)

// fixedBook returns a codebook whose single entry yields the given vector, so partition tests can
// control exactly what lands in the output.
func fixedBook(t *testing.T, dims int, values []uint32) *Codebook {
	t.Helper()
	c := &Codebook{
		Dimensions:    dims,
		Entries:       1,
		LookupType:    2,
		MinimumValue:  0,
		DeltaValue:    1,
		ValueBits:     8,
		LookupValues:  len(values),
		Multiplicands: values,
		lengths:       []uint8{1},
		single:        -1,
	}
	if err := c.buildTree(); err != nil {
		t.Fatalf("buildTree: %v", err)
	}
	return c
}

// TestPartitionInterleaveDiffersByType is the trap that separates residue 0 from residue 1.
//
// Vorbis I 8.3 and 8.4: type 0 strides its sub-vectors across the partition, type 1 writes them
// consecutively. Mistaking one for the other produces plausible-sounding noise rather than an error,
// and no reference stream in the corpus uses type 0, so only this test covers it.
func TestPartitionInterleaveDiffersByType(t *testing.T) {
	// A partition of 8 values, decoded by a 2-dimensional book that always returns [1, 2].
	book := fixedBook(t, 2, []uint32{1, 2})

	// Four codewords fill the partition; the book has one entry so any bits decode to it.
	data := make([]byte, 8)

	t.Run("type 0 strides", func(t *testing.T) {
		res := &Residue{Type: 0, PartitionSize: 8}
		v := make([]float32, 8)
		if err := res.decodePartition(bitio.NewLSBReader(data), book, v, 0, make([]float32, 2)); err != nil {
			t.Fatalf("decodePartition: %v", err)
		}
		// step = 8/2 = 4, so element j of each vector lands at i + j*4.
		want := []float32{1, 1, 1, 1, 2, 2, 2, 2}
		for i := range want {
			if v[i] != want[i] {
				t.Fatalf("got %v, want %v", v, want)
			}
		}
	})

	t.Run("type 1 is consecutive", func(t *testing.T) {
		res := &Residue{Type: 1, PartitionSize: 8}
		v := make([]float32, 8)
		if err := res.decodePartition(bitio.NewLSBReader(data), book, v, 0, make([]float32, 2)); err != nil {
			t.Fatalf("decodePartition: %v", err)
		}
		want := []float32{1, 2, 1, 2, 1, 2, 1, 2}
		for i := range want {
			if v[i] != want[i] {
				t.Fatalf("got %v, want %v", v, want)
			}
		}
	})
}

func TestPartitionAccumulates(t *testing.T) {
	// A partition is added to, not assigned: multiple passes accumulate into the same vector.
	book := fixedBook(t, 2, []uint32{3, 5})
	res := &Residue{Type: 1, PartitionSize: 4}

	v := make([]float32, 4)
	for range 3 {
		if err := res.decodePartition(bitio.NewLSBReader(make([]byte, 4)), book, v, 0, make([]float32, 2)); err != nil {
			t.Fatalf("decodePartition: %v", err)
		}
	}
	want := []float32{9, 15, 9, 15} // three passes of [3, 5]
	for i := range want {
		if v[i] != want[i] {
			t.Fatalf("got %v, want %v", v, want)
		}
	}
}

func TestPartitionWritesAtOffset(t *testing.T) {
	book := fixedBook(t, 2, []uint32{7, 7})
	res := &Residue{Type: 1, PartitionSize: 2}

	v := make([]float32, 6)
	if err := res.decodePartition(bitio.NewLSBReader(make([]byte, 4)), book, v, 2, make([]float32, 2)); err != nil {
		t.Fatalf("decodePartition: %v", err)
	}
	want := []float32{0, 0, 7, 7, 0, 0}
	for i := range want {
		if v[i] != want[i] {
			t.Fatalf("got %v, want %v", v, want)
		}
	}
}

func TestPartitionDoesNotWritePastEnd(t *testing.T) {
	book := fixedBook(t, 4, []uint32{1, 1, 1, 1})
	res := &Residue{Type: 1, PartitionSize: 8}

	// Offset near the end: writes beyond the vector must be dropped rather than panicking.
	v := make([]float32, 6)
	if err := res.decodePartition(bitio.NewLSBReader(make([]byte, 8)), book, v, 4, make([]float32, 4)); err != nil {
		t.Fatalf("decodePartition: %v", err)
	}
}

func TestPartitionRejectsSizeSmallerThanDimension(t *testing.T) {
	book := fixedBook(t, 8, make([]uint32, 8))
	res := &Residue{Type: 0, PartitionSize: 4}

	v := make([]float32, 8)
	if err := res.decodePartition(bitio.NewLSBReader(make([]byte, 8)), book, v, 0, make([]float32, 8)); err == nil {
		t.Error("accepted a partition smaller than the book dimension in type 0")
	}
}

// TestDecodeType2Deinterleaves covers Vorbis I 8.6.5: type 2 codes every channel as one long
// vector and splits it afterwards, where types 0 and 1 code each channel separately.
func TestDecodeType2Deinterleaves(t *testing.T) {
	// A value book returning [1..8] for its only entry, so the interleaved vector is known exactly.
	valueBook := fixedBook(t, 8, []uint32{1, 2, 3, 4, 5, 6, 7, 8})
	classBook := fixedBook(t, 1, []uint32{0})
	codebooks := []*Codebook{classBook, valueBook}

	res := &Residue{
		Type:            2,
		Begin:           0,
		End:             8, // channels * samples
		PartitionSize:   8,
		Classifications: 1,
		Classbook:       0,
		Books:           [][maxResiduePasses]int{{1, -1, -1, -1, -1, -1, -1, -1}},
	}

	out := [][]float32{make([]float32, 4), make([]float32, 4)}
	if err := res.Decode(bitio.NewLSBReader(make([]byte, 16)), codebooks, out, []bool{false, false}); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	// The flat vector is [1..8]; channel j takes every other value starting at j.
	want0 := []float32{1, 3, 5, 7}
	want1 := []float32{2, 4, 6, 8}
	for i := range want0 {
		if out[0][i] != want0[i] || out[1][i] != want1[i] {
			t.Fatalf("got ch0=%v ch1=%v, want %v and %v", out[0], out[1], want0, want1)
		}
	}
}

// TestDecodeType1KeepsChannelsSeparate is the contrast to the type 2 case above: the same books and
// partition applied per channel rather than interleaved.
func TestDecodeType1KeepsChannelsSeparate(t *testing.T) {
	valueBook := fixedBook(t, 4, []uint32{1, 2, 3, 4})
	classBook := fixedBook(t, 1, []uint32{0})
	codebooks := []*Codebook{classBook, valueBook}

	res := &Residue{
		Type:            1,
		Begin:           0,
		End:             4,
		PartitionSize:   4,
		Classifications: 1,
		Classbook:       0,
		Books:           [][maxResiduePasses]int{{1, -1, -1, -1, -1, -1, -1, -1}},
	}

	out := [][]float32{make([]float32, 4), make([]float32, 4)}
	if err := res.Decode(bitio.NewLSBReader(make([]byte, 16)), codebooks, out, []bool{false, false}); err != nil {
		t.Fatalf("Decode: %v", err)
	}

	want := []float32{1, 2, 3, 4}
	for j := range out {
		for i := range want {
			if out[j][i] != want[i] {
				t.Fatalf("channel %d = %v, want %v", j, out[j], want)
			}
		}
	}
}

func TestDecodeZeroesOutputFirst(t *testing.T) {
	res := &Residue{Type: 1, PartitionSize: 4, Begin: 0, End: 0}
	out := [][]float32{{9, 9, 9, 9}, {9, 9, 9, 9}}

	if err := res.Decode(bitio.NewLSBReader(nil), nil, out, []bool{false, false}); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for j, v := range out {
		for i := range v {
			if v[i] != 0 {
				t.Fatalf("channel %d sample %d = %v, want 0", j, i, v[i])
			}
		}
	}
}

// TestDecodeType2AllSkippedProducesSilence covers the rule that type 2 handles 'do not decode'
// differently: only when every channel is skipped does decode produce nothing.
func TestDecodeType2AllSkippedProducesSilence(t *testing.T) {
	res := &Residue{Type: 2, PartitionSize: 4, Begin: 0, End: 8}
	out := [][]float32{{1, 1, 1, 1}, {1, 1, 1, 1}}

	if err := res.Decode(bitio.NewLSBReader(nil), nil, out, []bool{true, true}); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	for j, v := range out {
		for i := range v {
			if v[i] != 0 {
				t.Fatalf("channel %d sample %d = %v, want 0", j, i, v[i])
			}
		}
	}
}

func TestDecodeEmptyChannelList(t *testing.T) {
	res := &Residue{Type: 1, PartitionSize: 4}
	if err := res.Decode(bitio.NewLSBReader(nil), nil, nil, nil); err != nil {
		t.Errorf("Decode with no channels: %v", err)
	}
}
