package celt

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestConformanceInverseMDCTMatchesReference compares the inverse transform against libopus.
//
// The transform folds windowing and time-domain aliasing into itself, and the folding is where an
// implementation goes wrong in ways that still sound like audio: an overlap applied at the wrong
// offset, or a mirrored half taken in the wrong direction, produces a plausible waveform that is not
// the right one. Only a comparison against the reference catches that, which is why the vectors come
// from testdata/gen_mdct.c built against celt/mdct.c itself.
func TestConformanceInverseMDCTMatchesReference(t *testing.T) {
	f, err := os.Open("testdata/mdct_vectors.txt")
	if err != nil {
		t.Fatalf("reading vectors: %v", err)
	}
	defer f.Close()

	tr := NewMDCT(960)
	var window []float32
	var in, want []float32
	var lm, short, n, blocks, shift int
	cases, windowed := 0, 0

	check := func() {
		if in == nil {
			return
		}
		got := make([]float32, len(want))
		tr.InverseFrame(in, got, lm, short == 1, 3)

		for i := range want {
			if math.Abs(float64(got[i]-want[i])) > 2e-4 {
				t.Fatalf("LM=%d short=%d sample %d: %v, reference has %v",
					lm, short, i, got[i], want[i])
			}
		}
		cases++
		in, want = nil, nil
	}

	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) == 0 || fields[0] == "#" {
			continue
		}
		num := func(i int) float64 {
			v, err := strconv.ParseFloat(fields[i], 64)
			if err != nil {
				t.Fatalf("parsing %q: %v", fields[i], err)
			}
			return v
		}
		atoi := func(i int) int {
			v, err := strconv.Atoi(fields[i])
			if err != nil {
				t.Fatalf("parsing %q: %v", fields[i], err)
			}
			return v
		}

		switch fields[0] {
		case "window":
			window = append(window, float32(num(2)))
		case "case":
			check()
			lm, short, n, blocks, shift = atoi(1), atoi(2), atoi(3), atoi(4), atoi(5)
			_, _ = blocks, shift
			in = make([]float32, 0, n)
		case "in":
			in = append(in, float32(num(1)))
		case "out":
			want = append(want, float32(num(1)))
		}
	}
	check()
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}

	// The window is the reference's own, so a mismatch there would show up as a wrong taper rather
	// than as a wrong transform.
	if len(window) != Overlap {
		t.Fatalf("reference window has %d entries, want %d", len(window), Overlap)
	}
	for i, w := range window {
		if math.Abs(float64(tr.Window()[i]-w)) > 1e-6 {
			t.Errorf("window[%d] = %v, reference has %v", i, tr.Window()[i], w)
		}
		if w > 1e-3 && w < 1-1e-3 {
			windowed++
		}
	}
	if windowed < Overlap/2 {
		t.Errorf("only %d window values are between zero and one; it is not a taper", windowed)
	}

	if cases != 8 {
		t.Fatalf("checked %d transform cases, want 8", cases)
	}
	t.Logf("matched the reference on %d transforms and all %d window values", cases, len(window))
}
