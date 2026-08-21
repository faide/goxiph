package vorbis

import (
	"reflect"
	"testing"

	"github.com/faide/goxiph/internal/bitio"
)

// sampleSetup is a configuration exercising every field the writer has to place: coupling, two
// submaps, two modes, a sparse book, a lattice book, and a floor class with and without subclasses.
func sampleSetup(t *testing.T) *Setup {
	t.Helper()

	// A book the floor's classes select from, and one the residue partitions use.
	classLengths, err := BuildLengths([]int{40, 20, 10, 5}, 8)
	if err != nil {
		t.Fatal(err)
	}
	classBook, err := NewCodebook(1, classLengths)
	if err != nil {
		t.Fatal(err)
	}

	sparse, err := NewCodebook(1, []uint8{1, 0, 3, 3, 3, 0, 4, 4})
	if err != nil {
		t.Fatal(err)
	}

	latticeLengths, err := BuildLengths([]int{90, 70, 55, 30, 20, 12, 6, 3, 1}, 12)
	if err != nil {
		t.Fatal(err)
	}
	lattice, err := NewCodebook(2, latticeLengths)
	if err != nil {
		t.Fatal(err)
	}
	values := lookup1Values(lattice.Entries, lattice.Dimensions)
	mult := make([]uint32, values)
	for i := range mult {
		mult[i] = uint32(i * 3)
	}
	if err := lattice.SetLookup(-0.75, 0.25, 5, true, mult); err != nil {
		t.Fatal(err)
	}

	return &Setup{
		Codebooks: []*Codebook{classBook, sparse, lattice},
		Floors: []*Floor{{
			Type: 1,
			One: &Floor1{
				PartitionClassList: []int{0, 0, 1},
				ClassDimensions:    []int{2, 3},
				ClassSubclasses:    []int{0, 2},
				ClassMasterbooks:   []int{-1, 0},
				SubclassBooks:      [][]int{{1}, {-1, 1, 1, 1}},
				Multiplier:         2,
				XList:              []int{0, 128, 14, 4, 58, 2, 40, 9, 70},
			},
		}},
		Residues: []*Residue{{
			Type:            2,
			Begin:           0,
			End:             128,
			PartitionSize:   16,
			Classifications: 4,
			Classbook:       0,
			Cascade:         []int{1, 3, 0, 9},
			// Only the lattice book has the value mapping a residue book needs.
			// A pass the cascade does not select carries no book, which the parser marks with -1.
			Books: [][maxResiduePasses]int{
				{2, -1, -1, -1, -1, -1, -1, -1},
				{2, 2, -1, -1, -1, -1, -1, -1},
				{-1, -1, -1, -1, -1, -1, -1, -1},
				{2, -1, -1, 2, -1, -1, -1, -1},
			},
		}},
		Mappings: []*Mapping{{
			CouplingSteps: []CouplingStep{{Magnitude: 0, Angle: 1}},
			Mux:           []int{0, 1},
			SubmapFloor:   []int{0, 0},
			SubmapResidue: []int{0, 0},
		}},
		Modes: []*Mode{{BlockFlag: false, Mapping: 0}, {BlockFlag: true, Mapping: 0}},
	}
}

// TestSetupSurvivesWriteAndParse checks that what the writer emits is what the parser reads back.
//
// The two halves are written from the same specification but not from each other, so a field placed
// at the wrong width or in the wrong order shows up here and nowhere else: a decoder reading its own
// encoder's output is the only test that covers the setup header end to end.
func TestSetupSurvivesWriteAndParse(t *testing.T) {
	want := sampleSetup(t)
	info := Info{Channels: 2, SampleRate: 44100, BlockSize0: 256, BlockSize1: 2048}

	packet, err := want.AppendTo(nil)
	if err != nil {
		t.Fatalf("writing: %v", err)
	}
	got, err := ParseSetup(packet, info)
	if err != nil {
		t.Fatalf("parsing back: %v", err)
	}

	if len(got.Codebooks) != len(want.Codebooks) {
		t.Fatalf("%d codebooks, want %d", len(got.Codebooks), len(want.Codebooks))
	}
	for i := range want.Codebooks {
		a, b := want.Codebooks[i], got.Codebooks[i]
		if a.Dimensions != b.Dimensions || a.Entries != b.Entries || a.LookupType != b.LookupType {
			t.Errorf("codebook %d: shape %d/%d/%d, want %d/%d/%d",
				i, b.Dimensions, b.Entries, b.LookupType, a.Dimensions, a.Entries, a.LookupType)
		}
		if !reflect.DeepEqual(a.lengths, b.lengths) {
			t.Errorf("codebook %d lengths: %v, want %v", i, b.lengths, a.lengths)
		}
		if !reflect.DeepEqual(a.Multiplicands, b.Multiplicands) {
			t.Errorf("codebook %d multiplicands: %v, want %v", i, b.Multiplicands, a.Multiplicands)
		}
		if a.LookupType != 0 && (a.ValueBits != b.ValueBits || a.SequenceP != b.SequenceP) {
			t.Errorf("codebook %d lookup: %d bits seq=%v, want %d seq=%v",
				i, b.ValueBits, b.SequenceP, a.ValueBits, a.SequenceP)
		}
	}

	if len(got.Floors) != 1 || got.Floors[0].Type != 1 {
		t.Fatalf("%d floors", len(got.Floors))
	}
	a, b := want.Floors[0].One, got.Floors[0].One
	for _, c := range []struct {
		name string
		x, y any
	}{
		{"partition classes", a.PartitionClassList, b.PartitionClassList},
		{"class dimensions", a.ClassDimensions, b.ClassDimensions},
		{"class subclasses", a.ClassSubclasses, b.ClassSubclasses},
		{"class masterbooks", a.ClassMasterbooks, b.ClassMasterbooks},
		{"subclass books", a.SubclassBooks, b.SubclassBooks},
		{"X list", a.XList, b.XList},
	} {
		if !reflect.DeepEqual(c.x, c.y) {
			t.Errorf("floor %s: %v, want %v", c.name, c.y, c.x)
		}
	}
	if a.Multiplier != b.Multiplier {
		t.Errorf("floor multiplier %d, want %d", b.Multiplier, a.Multiplier)
	}

	if len(got.Residues) != 1 {
		t.Fatalf("%d residues", len(got.Residues))
	}
	rw, rg := want.Residues[0], got.Residues[0]
	if rw.Type != rg.Type || rw.Begin != rg.Begin || rw.End != rg.End ||
		rw.PartitionSize != rg.PartitionSize || rw.Classifications != rg.Classifications ||
		rw.Classbook != rg.Classbook {
		t.Errorf("residue shape %+v, want %+v", rg, rw)
	}
	if !reflect.DeepEqual(rw.Cascade, rg.Cascade) {
		t.Errorf("residue cascade %v, want %v", rg.Cascade, rw.Cascade)
	}
	if !reflect.DeepEqual(rw.Books, rg.Books) {
		t.Errorf("residue books %v, want %v", rg.Books, rw.Books)
	}

	if !reflect.DeepEqual(want.Mappings, got.Mappings) {
		t.Errorf("mapping %+v, want %+v", got.Mappings[0], want.Mappings[0])
	}
	if !reflect.DeepEqual(want.Modes, got.Modes) {
		t.Errorf("modes %+v, want %+v", got.Modes, want.Modes)
	}
}

// TestCodebookCodewordsDecodeBack checks that the codewords a built book assigns are the ones a
// decoder reads out of it.
//
// A Huffman code is only useful if both sides walk the tree the same way, and the two do it by
// different means: the encoder emits a codeword from a table, the decoder descends bit by bit.
func TestCodebookCodewordsDecodeBack(t *testing.T) {
	lengths, err := BuildLengths([]int{100, 60, 40, 25, 12, 6, 3, 1}, 10)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewCodebook(1, lengths)
	if err != nil {
		t.Fatal(err)
	}

	w := bitio.NewLSBWriter()
	var wrote []int
	for entry := range c.Entries {
		if c.CodewordLength(entry) == 0 {
			continue
		}
		if err := c.WriteCodeword(w, entry); err != nil {
			t.Fatal(err)
		}
		wrote = append(wrote, entry)
	}
	w.AlignByte()

	r := bitio.NewLSBReader(w.Bytes())
	for i, want := range wrote {
		got, err := c.DecodeScalar(r)
		if err != nil {
			t.Fatalf("entry %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("codeword %d decoded to entry %d, want %d", i, got, want)
		}
	}
	t.Logf("%d codewords, %d bits, round-tripped", len(wrote), w.BitsWritten())
}
