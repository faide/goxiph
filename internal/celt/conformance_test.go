//go:build conformance

package celt

import (
	"bytes"
	"os"
	"regexp"
	"strconv"
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
