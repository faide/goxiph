//go:build conformance

package celt

import (
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
