package celt

import (
	"bufio"
	"fmt"
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

// analysisSignal is the same buffer the oracle builds: one period of noise repeated, plus a little
// fresh noise, so the pitch has a right answer and the filter fit has something to shape.
//
// A sine would do as well but for the fixture, whose two sides must agree to the last bit; this is
// built from a shift register and single-precision arithmetic, so they do.
func analysisSignal(length int) [][]float32 {
	const period = 137
	var base [period]float32
	s := uint32(12345)
	noise := func() float32 {
		s = 1664525*s + 1013904223
		return float32(int32(s>>8)-8388608) / 8388608
	}
	for i := range base {
		base[i] = noise()
	}

	ch := [][]float32{make([]float32, length), make([]float32, length)}
	for i := range length {
		n := noise()
		ch[0][i] = 0.4*base[i%period] + 0.05*n
		ch[1][i] = 0.3*base[(i+40)%period] + 0.05*n
	}
	return ch
}

// TestConcealmentAnalysisMatchesReference checks the pitch search and the filter fit on their own.
//
// Concealment is an extrapolation, so its output is not a thing the format pins down; what has to be
// right is the analysis it rests on. A pitch off by one repeats the wrong stretch of signal, and a
// filter fitted differently shapes it differently. Both are checked here against the reference,
// where the whole concealed frame would only say that something differed.
// The two sides agree bit for bit up to the nine digits the vector files carry, so what is left is
// headroom for a platform that contracts a multiply and an add into one instruction. Coefficients
// are of order one and the signals a little larger, which is the whole of the difference between
// the two bounds.
const (
	coefTolerance   = 1e-7
	signalTolerance = 1e-6
)

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
		if worst > signalTolerance {
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
		ac[i] -= ac[i] * (0.008 * float32(i)) * (0.008 * float32(i))
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
		if worst > coefTolerance {
			t.Errorf("prediction coefficients differ by %v", worst)
		}
	}
	t.Logf("pitch index %d and %d coefficients match the reference", found, lpcOrder)
}

// TestConcealmentStagesMatchReference runs the whole extrapolation on a known buffer and checks it
// stage by stage.
//
// The analysis test above covers the fit; this covers what the fit is then used for. Each stage is
// checked separately because the last of them runs a twenty-fourth-order recursive filter over more
// than a thousand samples, so a small difference anywhere upstream arrives at the output large and
// unattributable.
func TestConcealmentStagesMatchReference(t *testing.T) {
	want := readAnalysisVectors(t, "testdata/plc_stages.txt")

	const length = decodeBufferSize
	sig := analysisSignal(length + Overlap)
	mem := [][]float32{make([]float32, length+128), make([]float32, length+128)}
	copy(mem[0], sig[0])
	copy(mem[1], sig[1])

	lp := make([]float32, length>>1)
	pitchDownsample(mem, lp, length)
	found := pitchSearch(lp[pitchSearchOffset>>1:], lp,
		length-pitchSearchOffset, pitchSearchOffset-pitchSearchMin)
	pitch := pitchSearchOffset - found
	if float64(pitch) != want["pitch"][0] {
		t.Fatalf("pitch %d, reference has %v", pitch, want["pitch"][0])
	}

	worstAgainst := func(name string, got []float32) float64 {
		t.Helper()
		ref, ok := want[name]
		if !ok {
			t.Fatalf("reference has no %s", name)
		}
		if len(got) != len(ref) {
			t.Fatalf("%s holds %d values, reference has %d", name, len(got), len(ref))
		}
		var worst float64
		for i := range ref {
			worst = math.Max(worst, math.Abs(float64(got[i])-ref[i]))
		}
		return worst
	}
	check := func(name string, got []float32) {
		t.Helper()
		if worst := worstAgainst(name, got); worst > signalTolerance {
			t.Errorf("%s differs by %v", name, worst)
		}
	}
	checkCoefs := func(name string, got []float32) {
		t.Helper()
		if worst := worstAgainst(name, got); worst > coefTolerance {
			t.Errorf("%s differs by %v", name, worst)
		}
	}

	window := Window(Overlap)
	for c := range 2 {
		out := mem[c][length-combMaxPeriod:]
		exc := make([]float32, combMaxPeriod)
		copy(exc, out[:combMaxPeriod])

		ac := autocorrelate(exc, window, Overlap, lpcOrder)
		ac[0] *= 1.0001
		for i := 1; i <= lpcOrder; i++ {
			ac[i] -= ac[i] * (0.008 * float32(i)) * (0.008 * float32(i))
		}
		lpc := levinson(ac, lpcOrder)
		checkCoefs(fmt.Sprintf("lpc%d", c), lpc)

		filterMem := make([]float32, lpcOrder)
		for i := range lpcOrder {
			filterMem[i] = out[combMaxPeriod-1-i]
		}
		fir(exc, lpc, exc, lpcOrder, filterMem)
		check(fmt.Sprintf("exc%d", c), exc)

		decay := decayRate(exc, pitch)
		if d := math.Abs(float64(decay) - want[fmt.Sprintf("decay%d", c)][0]); d > coefTolerance {
			t.Errorf("channel %d decay differs by %v", c, d)
		}

		const frame = 960
		e := make([]float32, frame+2*Overlap)
		offset := combMaxPeriod - pitch
		var s1 float32
		for i := range e {
			if offset+i >= combMaxPeriod {
				offset -= pitch
				decay *= decay
			}
			e[i] = decay * exc[offset+i]
			s1 += out[offset+i] * out[offset+i]
		}
		check(fmt.Sprintf("ecopy%d", c), e)
		if d := math.Abs(float64(s1) - want[fmt.Sprintf("s1_%d", c)][0]); d > signalTolerance*float64(s1) {
			t.Errorf("channel %d energy differs by %v", c, d)
		}

		for i := range lpcOrder {
			filterMem[i] = out[combMaxPeriod-1-i]
		}
		iir(e, lpc, e, lpcOrder, filterMem)
		check(fmt.Sprintf("eiir%d", c), e)
	}
}
