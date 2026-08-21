//go:build conformance

package celt

import (
	"bytes"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// referenceSource is where `mise run specs:fetch` plus the extraction step leave the RFC 6716
// reference implementation.
const referenceSource = "../../.specs/opus-rfc6716"

// TestConformanceEnergyModelMatchesReference checks the transcribed probability table against the
// source it came from.
//
// The table was produced by a generator rather than typed, which moves the risk from transcription
// to the generator itself. Reading the reference again here and comparing every one of the 336
// values closes that gap; nothing else in the package can, because the table has no derivation.
func TestConformanceEnergyModelMatchesReference(t *testing.T) {
	raw, err := os.ReadFile(referenceSource + "/celt/quant_bands.c")
	if err != nil {
		t.Skipf("reference implementation not extracted: %v", err)
	}

	body := regexp.MustCompile(`(?s)e_prob_model\[4]\[2]\[42] = \{(.*?)\n\};`).FindSubmatch(raw)
	if body == nil {
		t.Fatal("could not find e_prob_model in the reference")
	}
	stripped := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAll(body[1], nil)

	var want []int
	for _, m := range regexp.MustCompile(`\d+`).FindAll(stripped, -1) {
		v, err := strconv.Atoi(string(m))
		if err != nil {
			t.Fatalf("parsing %q: %v", m, err)
		}
		want = append(want, v)
	}
	if len(want) != 4*2*42 {
		t.Fatalf("reference holds %d values, want %d", len(want), 4*2*42)
	}

	i := 0
	for lm := range 4 {
		for intra := range 2 {
			for b := range 42 {
				if got := int(energyProbModel[lm][intra][b]); got != want[i] {
					t.Fatalf("frame %d intra=%d entry %d: have %d, reference has %d",
						lm, intra, b, got, want[i])
				}
				i++
			}
		}
	}
	t.Logf("all %d entries match the reference implementation", len(want))
}

// TestConformanceBandLayoutMatchesReference cross-checks the band table, which was derived from
// RFC 6716 table 55 rather than copied.
//
// Agreement between an independent derivation and the reference is what makes the derivation
// trustworthy; either alone would only be an assertion.
func TestConformanceBandLayoutMatchesReference(t *testing.T) {
	raw, err := os.ReadFile(referenceSource + "/celt/modes.c")
	if err != nil {
		t.Skipf("reference implementation not extracted: %v", err)
	}

	body := regexp.MustCompile(`(?s)eband5ms\[] = \{(.*?)\n\};`).FindSubmatch(raw)
	if body == nil {
		t.Fatal("could not find eband5ms in the reference")
	}
	stripped := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAll(body[1], nil)

	var want []int
	for _, m := range regexp.MustCompile(`\d+`).FindAll(stripped, -1) {
		v, _ := strconv.Atoi(string(m))
		want = append(want, v)
	}
	if len(want) != NumBands+1 {
		t.Fatalf("reference holds %d edges, want %d", len(want), NumBands+1)
	}
	for i, w := range want {
		if BandEdges[i] != w {
			t.Fatalf("band edge %d: derived %d, reference has %d", i, BandEdges[i], w)
		}
	}
	t.Logf("all %d band edges match the reference implementation", len(want))
}

// TestConformancePredictionCoefficientsMatchReference checks the four prediction weights and the two
// beta sets, which the specification describes only as living in the reference.
func TestConformancePredictionCoefficientsMatchReference(t *testing.T) {
	raw, err := os.ReadFile(referenceSource + "/celt/quant_bands.c")
	if err != nil {
		t.Skipf("reference implementation not extracted: %v", err)
	}

	find := func(name string, want [4]float32) {
		t.Helper()
		re := regexp.MustCompile(name + `\[4] = \{([^}]*)\}`)
		m := re.FindSubmatch(raw)
		if m == nil {
			t.Fatalf("could not find %s in the reference", name)
		}
		nums := regexp.MustCompile(`\d+`).FindAll(m[1], -1)
		// The fixed-point form lists the raw numerators, which is the first match of each pair.
		for i := range 4 {
			v, _ := strconv.Atoi(string(nums[i]))
			if got := float32(v) / 32768; got != want[i] {
				t.Errorf("%s[%d] = %v, reference has %d/32768 = %v", name, i, want[i], v, got)
			}
		}
	}
	find("pred_coef", predCoef)
	find("beta_coef", betaCoef)

	m := regexp.MustCompile(`beta_intra = (\d+)`).FindSubmatch(raw)
	if m == nil {
		t.Fatal("could not find beta_intra in the reference")
	}
	v, _ := strconv.Atoi(string(m[1]))
	if got := float32(v) / 32768; got != betaIntra {
		t.Errorf("betaIntra = %v, reference has %d/32768 = %v", betaIntra, v, got)
	}
}

// TestConformanceAllocationTablesMatchReference checks the three allocation tables against the
// source they were generated from.
//
// RFC 6716 section 4.3.3 prints the static allocation table but not the caps or the log widths, and
// it states plainly that the allocation must be reproduced exactly or the output is corrupted. That
// makes a wrong entry here silent until audio comes out wrong, so it is checked directly.
func TestConformanceAllocationTablesMatchReference(t *testing.T) {
	nums := func(path, decl string, want int) []int {
		t.Helper()
		raw, err := os.ReadFile(referenceSource + path)
		if err != nil {
			t.Skipf("reference implementation not extracted: %v", err)
		}
		i := bytesIndex(raw, decl)
		if i < 0 {
			t.Fatalf("could not find %q in %s", decl, path)
		}
		j := bytesIndex(raw[i:], "};")
		body := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAll(raw[i+len(decl):i+j], nil)

		var out []int
		for _, m := range regexp.MustCompile(`-?\d+`).FindAll(body, -1) {
			v, err := strconv.Atoi(string(m))
			if err != nil {
				t.Fatalf("parsing %q: %v", m, err)
			}
			out = append(out, v)
		}
		if len(out) != want {
			t.Fatalf("%s holds %d values, want %d", decl, len(out), want)
		}
		return out
	}

	t.Run("band allocation", func(t *testing.T) {
		want := nums("/celt/modes.c", "band_allocation[] = {", 11*NumBands)
		for q := range 11 {
			for b := range NumBands {
				if got := int(bandAllocation[q][b]); got != want[q*NumBands+b] {
					t.Fatalf("quality %d band %d: have %d, reference has %d",
						q, b, got, want[q*NumBands+b])
				}
			}
		}
		t.Logf("all %d allocation entries match", len(want))
	})

	t.Run("caps", func(t *testing.T) {
		want := nums("/celt/static_modes_float.h", "cache_caps50[168] = {", 168)
		for i := range 8 {
			for b := range NumBands {
				if got := int(cacheCaps[i][b]); got != want[i*NumBands+b] {
					t.Fatalf("row %d band %d: have %d, reference has %d",
						i, b, got, want[i*NumBands+b])
				}
			}
		}
		t.Logf("all %d cap entries match", len(want))
	})

	t.Run("log widths", func(t *testing.T) {
		want := nums("/celt/static_modes_float.h", "logN400[21] = {", NumBands)
		for b := range NumBands {
			if logN[b] != want[b] {
				t.Fatalf("band %d: have %d, reference has %d", b, logN[b], want[b])
			}
		}
	})

	t.Run("log2 fraction table", func(t *testing.T) {
		want := nums("/celt/rate.c", "LOG2_FRAC_TABLE[24]={", 24)
		for i := range 24 {
			if int(log2FracTable[i]) != want[i] {
				t.Fatalf("entry %d: have %d, reference has %d", i, log2FracTable[i], want[i])
			}
		}
	})

	t.Run("trim distribution", func(t *testing.T) {
		want := nums("/celt/celt.c", "trim_icdf[11] = {", 11)
		for i := range 11 {
			if int(trimICDF[i]) != want[i] {
				t.Fatalf("entry %d: have %d, reference has %d", i, trimICDF[i], want[i])
			}
		}
	})
}

// bytesIndex is strings.Index over a byte slice without the conversion.
func bytesIndex(haystack []byte, needle string) int {
	return bytes.Index(haystack, []byte(needle))
}

// TestConformanceSpreadTablesMatchReference checks the spreading constants against their source.
//
// Both are short enough to have been typed, which is exactly when a transposed digit survives every
// other test: the rotation still round-trips and still preserves the norm at the wrong angle.
func TestConformanceSpreadTablesMatchReference(t *testing.T) {
	readInts := func(file, pattern string, want int) []int {
		raw, err := os.ReadFile(referenceSource + "/celt/" + file)
		if err != nil {
			t.Skipf("reference implementation not extracted: %v", err)
		}
		body := regexp.MustCompile(pattern).FindSubmatch(raw)
		if body == nil {
			t.Fatalf("could not find %s in %s", pattern, file)
		}
		var out []int
		for _, m := range regexp.MustCompile(`\d+`).FindAll(body[1], -1) {
			v, err := strconv.Atoi(string(m))
			if err != nil {
				t.Fatalf("parsing %q: %v", m, err)
			}
			out = append(out, v)
		}
		if len(out) != want {
			t.Fatalf("%s holds %d values, want %d", file, len(out), want)
		}
		return out
	}

	icdf := readInts("celt.c", `spread_icdf\[4] *= *\{(.*?)\};`, 4)
	for i, w := range icdf {
		if got := int(spreadICDF[i]); got != w {
			t.Errorf("spreadICDF[%d] = %d, reference has %d", i, got, w)
		}
	}

	factor := readInts("vq.c", `SPREAD_FACTOR\[3] *= *\{(.*?)\};`, 3)
	for i, w := range factor {
		if got := spreadFactor[i]; got != w {
			t.Errorf("spreadFactor[%d] = %d, reference has %d", i, got, w)
		}
	}
	t.Logf("spreading tables match: icdf %v, factor %v", icdf, factor)
}

// TestConformancePulseCacheMatchesReference checks the computed cost curves against the table the
// reference ships.
//
// The reference stores 392 bytes of precomputed costs plus a 105-entry index. Those are computed
// here instead, from the same recurrence the quantiser uses, so this is what says the computation
// reproduces the table exactly. Getting it wrong would charge bands the wrong number of bits and
// desynchronise every stream.
func TestConformancePulseCacheMatchesReference(t *testing.T) {
	raw, err := os.ReadFile(referenceSource + "/celt/static_modes_float.h")
	if err != nil {
		t.Skipf("reference implementation not extracted: %v", err)
	}

	read := func(pattern string, want int) []int {
		body := regexp.MustCompile(pattern).FindSubmatch(raw)
		if body == nil {
			t.Fatalf("could not find %s in the reference", pattern)
		}
		var out []int
		for _, m := range regexp.MustCompile(`-?\d+`).FindAll(body[1], -1) {
			v, err := strconv.Atoi(string(m))
			if err != nil {
				t.Fatalf("parsing %q: %v", m, err)
			}
			out = append(out, v)
		}
		if len(out) != want {
			t.Fatalf("reference holds %d values, want %d", len(out), want)
		}
		return out
	}

	wantIndex := read(`(?s)cache_index50\[105] = \{(.*?)\};`, 105)
	wantBits := read(`(?s)cache_bits50\[392] = \{(.*?)\};`, 392)

	c := newPulseCache()
	if len(c.index) != len(wantIndex) {
		t.Fatalf("computed %d index entries, reference has %d", len(c.index), len(wantIndex))
	}
	if len(c.bits) != len(wantBits) {
		t.Fatalf("computed %d cost bytes, reference has %d", len(c.bits), len(wantBits))
	}
	for i, w := range wantIndex {
		if got := int(c.index[i]); got != w {
			t.Fatalf("index[%d] (frame %d band %d): computed %d, reference has %d",
				i, i/NumBands, i%NumBands, got, w)
		}
	}
	for i, w := range wantBits {
		if got := int(c.bits[i]); got != w {
			t.Fatalf("cost byte %d: computed %d, reference has %d", i, got, w)
		}
	}
	t.Logf("all %d index entries and %d cost bytes match, with no table transcribed",
		len(wantIndex), len(wantBits))
}

// TestConformanceLogNIsDerivable checks the per-band log-width table against the expression the
// reference computes it from.
//
// It is transcribed in alloc.go and checked entry by entry elsewhere, so agreeing with log2Frac here
// is an independent statement that the fixed-point logarithm rounds the way the format needs.
func TestConformanceLogNIsDerivable(t *testing.T) {
	for i := range NumBands {
		width := uint32(BandEdges[i+1] - BandEdges[i])
		if got, want := int(logN[i]), log2Frac(width, BitRes); got != want {
			t.Errorf("logN[%d]: table has %d, log2Frac of width %d gives %d", i, got, width, want)
		}
	}
	t.Logf("all %d log-widths agree with log2Frac", NumBands)
}

// TestConformanceBandMeansMatchTheFloatTable checks the mean energies against the reference's own
// float copy of them.
//
// The integers are kept and divided here rather than transcribing 25 floats. That is only safe if
// the division is the right one, which is what this compares: the reference ships both forms, so
// they check each other.
func TestConformanceBandMeansMatchTheFloatTable(t *testing.T) {
	raw, err := os.ReadFile(referenceSource + "/celt/quant_bands.c")
	if err != nil {
		t.Skipf("reference implementation not extracted: %v", err)
	}

	body := regexp.MustCompile(`(?s)opus_val16 eMeans\[25] = \{(.*?)\};`).FindSubmatch(raw)
	if body == nil {
		t.Fatal("could not find the float eMeans table in the reference")
	}
	matches := regexp.MustCompile(`[\d.]+f`).FindAll(body[1], -1)
	if len(matches) != 25 {
		t.Fatalf("reference holds %d float means, want 25", len(matches))
	}

	for i, m := range matches {
		want, err := strconv.ParseFloat(strings.TrimSuffix(string(m), "f"), 32)
		if err != nil {
			t.Fatalf("parsing %q: %v", m, err)
		}
		if got := float64(float32(eMeansQ6[i]) / 16); got != want {
			t.Errorf("band %d: %d/16 = %v, reference float is %v", i, eMeansQ6[i], got, want)
		}
	}
	t.Logf("all 25 band means match the reference float table")
}
