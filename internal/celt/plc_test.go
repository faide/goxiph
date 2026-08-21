package celt

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// readAnalysisVectors parses the standalone dump of the concealment analysis.
func readAnalysisVectors(t *testing.T, path string) map[string][]float64 {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Skipf("no analysis vectors: %v", err)
	}
	defer f.Close()

	out := map[string][]float64{}
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<22), 1<<22)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) < 3 {
			continue
		}
		var v []float64
		for _, x := range fields[2:] {
			y, err := strconv.ParseFloat(x, 64)
			if err != nil {
				t.Fatalf("parsing %q: %v", x, err)
			}
			v = append(v, y)
		}
		out[fields[0]] = v
	}
	return out
}

// analysisSignal is the same buffer the oracle builds: a tone at a known period with a little noise,
// so the pitch has a right answer and the filter fit has something to shape.
func analysisSignal(length int) [][]float32 {
	ch := [][]float32{make([]float32, length), make([]float32, length)}
	s := uint32(12345)
	for i := range length {
		s = 1664525*s + 1013904223
		n := float64(int32(s>>8)-8388608) / 8388608.0
		ch[0][i] = float32(0.4*math.Sin(2*math.Pi*float64(i)/137) + 0.05*n)
		ch[1][i] = float32(0.3*math.Sin(2*math.Pi*float64(i)/137+0.7) + 0.05*n)
	}
	return ch
}

// TestConcealmentAnalysisMatchesReference checks the pitch search and the filter fit on their own.
//
// Concealment is an extrapolation, so its output is not a thing the format pins down; what has to be
// right is the analysis it rests on. A pitch off by one repeats the wrong stretch of signal, and a
// filter fitted differently shapes it differently. Both are checked here against the reference,
// where the whole concealed frame would only say that something differed.
func TestConcealmentAnalysisMatchesReference(t *testing.T) {
	want := readAnalysisVectors(t, "testdata/plc_analysis.txt")
	const length = 2048

	ch := analysisSignal(length)
	lp := make([]float32, length/2)
	pitchDownsample(ch, lp, length)

	if got, ok := want["downsampled"]; ok {
		if len(got) != length/2 {
			t.Fatalf("reference holds %d downsampled values, want %d", len(got), length/2)
		}
		var worst float64
		for i := range got {
			worst = math.Max(worst, math.Abs(float64(lp[i])-got[i]))
		}
		if worst > 1e-4 {
			t.Errorf("downsampled signal differs by %v", worst)
		}
	}

	found := pitchSearch(lp[pitchSearchOffset>>1:], lp,
		length-pitchSearchOffset, pitchSearchOffset-pitchSearchMin)
	if idx, ok := want["pitchindex"]; ok {
		if float64(found) != idx[0] {
			t.Errorf("pitch index %d, reference has %v", found, idx[0])
		}
	}

	ac := autocorrelate(lp, nil, 0, lpcOrder)
	ac[0] *= 1.0001
	for i := 1; i <= lpcOrder; i++ {
		ac[i] -= ac[i] * (0.008 * float64(i)) * (0.008 * float64(i))
	}
	lpc := levinson(ac, lpcOrder)
	if got, ok := want["lpc"]; ok {
		if len(got) != lpcOrder {
			t.Fatalf("reference holds %d coefficients, want %d", len(got), lpcOrder)
		}
		var worst float64
		for i := range got {
			worst = math.Max(worst, math.Abs(float64(lpc[i])-got[i]))
		}
		if worst > 1e-5 {
			t.Errorf("prediction coefficients differ by %v", worst)
		}
	}
	t.Logf("pitch index %d and %d coefficients match the reference", found, lpcOrder)
}
