package vorbis

import (
	"testing"
)

// TestRenderLineIsIntegerDDA pins the line algorithm of Vorbis I 9.2.7. A floating-point equivalent
// produces values that are off by one at the rounding boundaries, which passes casual inspection
// and fails conformance.
func TestRenderLineIsIntegerDDA(t *testing.T) {
	cases := []struct {
		name                   string
		x0, y0, x1, y1, length int
		want                   []int
	}{
		{"unit slope", 0, 0, 4, 4, 4, []int{0, 1, 2, 3}},
		{"shallow slope", 0, 0, 3, 1, 3, []int{0, 0, 0}},
		{"negative unit slope", 0, 4, 4, 0, 4, []int{4, 3, 2, 1}},
		{"flat", 0, 7, 5, 7, 5, []int{7, 7, 7, 7, 7}},
		{"steep", 0, 0, 2, 10, 2, []int{0, 5}},
		{"single point", 0, 3, 1, 9, 1, []int{3}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := make([]int, c.length)
			renderLine(c.x0, c.y0, c.x1, c.y1, v)
			for i := range c.want {
				if v[i] != c.want[i] {
					t.Errorf("got %v, want %v", v, c.want)
					return
				}
			}
		})
	}
}

// TestRenderLineNegativeDivisionRoundsTowardZero covers the one arithmetic detail the specification
// calls out: integer division must round toward zero for negative values as well as positive.
func TestRenderLineNegativeDivisionRoundsTowardZero(t *testing.T) {
	// dy/adx is -3/2. Rounding toward zero gives base -1; flooring would give -2 and drop the line
	// twice as fast.
	v := make([]int, 2)
	renderLine(0, 0, 2, -3, v)
	if v[0] != 0 {
		t.Fatalf("v[0] = %d, want 0", v[0])
	}
	if v[1] != -1 && v[1] != -2 {
		t.Fatalf("v[1] = %d, want -1 or -2", v[1])
	}
	if v[1] == -2 {
		t.Error("integer division floored instead of rounding toward zero")
	}
}

func TestRenderLineDoesNotWriteOutOfRange(t *testing.T) {
	v := make([]int, 4)
	// x1 beyond the buffer must not panic.
	renderLine(0, 0, 100, 100, v)
	// Negative x0 must not panic.
	renderLine(-5, 0, 3, 3, v)
	// Zero-width segment is a no-op.
	before := append([]int(nil), v...)
	renderLine(2, 9, 2, 9, v)
	for i := range v {
		if v[i] != before[i] {
			t.Error("a zero-width segment modified the curve")
			break
		}
	}
}

// TestRenderPoint checks the direct solve of Vorbis I 9.2.6.
func TestRenderPoint(t *testing.T) {
	cases := []struct {
		x0, y0, x1, y1, x, want int
	}{
		{0, 0, 10, 10, 5, 5},
		{0, 0, 10, 10, 0, 0},
		{0, 0, 10, 10, 10, 10},
		{0, 10, 10, 0, 5, 5},
		{0, 0, 3, 1, 1, 0}, // integer division truncates
		{0, 0, 3, 2, 2, 1}, // 2*2/3 == 1
		{5, 4, 5, 9, 5, 4}, // zero width returns y0
	}
	for _, c := range cases {
		if got := renderPoint(c.x0, c.y0, c.x1, c.y1, c.x); got != c.want {
			t.Errorf("renderPoint(%d,%d,%d,%d,%d) = %d, want %d",
				c.x0, c.y0, c.x1, c.y1, c.x, got, c.want)
		}
	}
}

func TestNeighbors(t *testing.T) {
	// The specification's neighbour search looks only at positions before x.
	v := []int{0, 128, 64, 32, 96}

	if got := lowNeighbor(v, 2); got != 0 {
		t.Errorf("lowNeighbor(v, 2) = %d, want 0", got)
	}
	if got := highNeighbor(v, 2); got != 1 {
		t.Errorf("highNeighbor(v, 2) = %d, want 1", got)
	}
	if got := lowNeighbor(v, 3); got != 0 {
		t.Errorf("lowNeighbor(v, 3) = %d, want 0", got)
	}
	if got := highNeighbor(v, 3); got != 2 {
		t.Errorf("highNeighbor(v, 3) = %d, want 2", got)
	}
	if got := lowNeighbor(v, 4); got != 2 {
		t.Errorf("lowNeighbor(v, 4) = %d, want 2", got)
	}
	if got := highNeighbor(v, 4); got != 1 {
		t.Errorf("highNeighbor(v, 4) = %d, want 1", got)
	}
}

// TestInverseDBTable sanity-checks the transcribed table from Vorbis I 10.1.
func TestInverseDBTable(t *testing.T) {
	if len(floor1InverseDBTable) != 256 {
		t.Fatalf("table has %d entries, want 256", len(floor1InverseDBTable))
	}
	if floor1InverseDBTable[0] != 1.0649863e-07 {
		t.Errorf("first entry = %v", floor1InverseDBTable[0])
	}
	if floor1InverseDBTable[255] != 1.0 {
		t.Errorf("last entry = %v, want 1.0", floor1InverseDBTable[255])
	}
	// The table is an amplitude ramp, so it must rise without exception.
	for i := 1; i < 256; i++ {
		if floor1InverseDBTable[i] <= floor1InverseDBTable[i-1] {
			t.Fatalf("entry %d (%v) is not greater than entry %d (%v)",
				i, floor1InverseDBTable[i], i-1, floor1InverseDBTable[i-1])
		}
	}
}

func TestBuildSortOrder(t *testing.T) {
	f := &Floor1{XList: []int{0, 128, 64, 16, 96, 32}}
	f.buildSortOrder()

	want := []int{0, 3, 5, 2, 4, 1} // indices of 0, 16, 32, 64, 96, 128
	for i := range want {
		if f.sortedOrder[i] != want[i] {
			t.Fatalf("sortedOrder = %v, want %v", f.sortedOrder, want)
		}
	}
	// The sort must be ascending in X.
	for i := 1; i < len(f.sortedOrder); i++ {
		if f.XList[f.sortedOrder[i-1]] > f.XList[f.sortedOrder[i]] {
			t.Fatalf("sortedOrder is not ascending in X: %v", f.sortedOrder)
		}
	}
}

// TestSynthesizeFlatFloor checks the simplest end-to-end curve: two equal endpoints give a constant
// amplitude across the whole spectrum.
func TestSynthesizeFlatFloor(t *testing.T) {
	f := &Floor1{
		PartitionClassList: nil,
		Multiplier:         1,
		XList:              []int{0, 128},
	}
	f.buildSortOrder()

	st := newFloor1State(f.Values(), 128)
	st.y[0] = 100
	st.y[1] = 100

	out := make([]float32, 128)
	f.Synthesize(st, out)

	want := floor1InverseDBTable[100]
	for i, v := range out {
		if v != want {
			t.Fatalf("sample %d = %v, want %v (flat floor)", i, v, want)
		}
	}
}

func TestSynthesizeClampsOutOfRange(t *testing.T) {
	f := &Floor1{Multiplier: 4, XList: []int{0, 64}}
	f.buildSortOrder()

	st := newFloor1State(f.Values(), 64)
	// Multiplier 4 gives range 64; these exceed it and must be clamped rather than indexing past
	// the end of the amplitude table.
	st.y[0] = 10000
	st.y[1] = -5000

	out := make([]float32, 64)
	f.Synthesize(st, out) // must not panic

	for i, v := range out {
		if v < 0 || v > 1 {
			t.Fatalf("sample %d = %v, outside the table range", i, v)
		}
	}
}

func BenchmarkFloor1Synthesize(b *testing.B) {
	f := &Floor1{Multiplier: 2, XList: []int{0, 1024, 128, 256, 512, 64}}
	f.buildSortOrder()
	st := newFloor1State(f.Values(), 1024)
	for i := range st.y {
		st.y[i] = 50 + i*7
	}
	out := make([]float32, 1024)

	b.ReportAllocs()
	for b.Loop() {
		f.Synthesize(st, out)
	}
}
