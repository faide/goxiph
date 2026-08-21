//go:build conformance

package silk

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const referenceSource = "../../.specs/opus-rfc6716/silk"

// TestConformanceTablesMatchReference checks every generated table against the source it came from.
//
// tables.go is produced by a tool rather than typed, which moves the risk from transcription to the
// tool: a dimension resolved wrongly or a declaration matched too greedily would put plausible
// numbers in the wrong places. This reads both files again and compares them value by value, so the
// committed artefact is checked rather than trusted.
//
// The pairing comes from the provenance comment each table carries, so adding a table to the
// generator brings it under this check without touching the test.
func TestConformanceTablesMatchReference(t *testing.T) {
	generated, err := os.ReadFile("tables.go")
	if err != nil {
		t.Fatalf("reading tables.go: %v", err)
	}

	files, err := filepath.Glob(filepath.Join(referenceSource, "*.c"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skipf("reference implementation not extracted in %s", referenceSource)
	}

	var reference strings.Builder
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		reference.WriteString(stripComments(string(raw)))
	}
	refSrc := reference.String()

	blockRe := regexp.MustCompile(
		`(?s)// Transcribed from (\w+) of the reference implementation\.\nvar (\w+) = (?:\[\d+\])+\w+\{(.*?)\n\}\n`)
	numRe := regexp.MustCompile(`-?\d+`)

	blocks := blockRe.FindAllStringSubmatch(string(generated), -1)
	if len(blocks) == 0 {
		t.Fatal("no generated tables found; the provenance comment may have changed shape")
	}

	total := 0
	for _, b := range blocks {
		ref, name, body := b[1], b[2], b[3]

		declRe := regexp.MustCompile(`(?s)const\s+opus_u?int\d+\s+` + regexp.QuoteMeta(ref) +
			`\s*(?:\[[^\]]*\]\s*)+=\s*\{(.*?)\};`)
		m := declRe.FindStringSubmatch(refSrc)
		if m == nil {
			t.Errorf("%s: %s not found in the reference", name, ref)
			continue
		}

		want := referenceValues(t, m[1])
		got := numRe.FindAllString(body, -1)
		if len(got) != len(want) {
			t.Errorf("%s: generated %d values, reference has %d", name, len(got), len(want))
			continue
		}
		for i := range want {
			a, err1 := strconv.Atoi(got[i])
			b, err2 := strconv.Atoi(want[i])
			if err1 != nil || err2 != nil {
				t.Fatalf("%s: parsing entry %d", name, i)
			}
			if a != b {
				t.Fatalf("%s entry %d: generated %d, reference has %d", name, i, a, b)
			}
		}
		total += len(want)
	}

	if len(blocks) < 50 {
		t.Errorf("only %d tables were checked; the generator produces more", len(blocks))
	}
	t.Logf("%d tables, %d values, all matching the reference", len(blocks), total)
}

func stripComments(s string) string {
	s = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(s, "")
	return regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(s, "")
}

// referenceValues splits a reference initialiser into its values.
//
// An entry may be written as a subtraction rather than as a literal, which a scan for digits would
// read as two values. This evaluates them with its own arithmetic rather than sharing the
// generator's: a test that reused the generator's parsing would agree with it whether or not either
// was right.
func referenceValues(t *testing.T, body string) []string {
	t.Helper()

	body = strings.NewReplacer("{", "", "}", "", "\n", " ").Replace(body)
	var out []string

	for _, field := range strings.Split(body, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		terms := regexp.MustCompile(`([+-]?\s*\d+)`).FindAllString(field, -1)
		total := 0
		for i, term := range terms {
			v, err := strconv.Atoi(strings.ReplaceAll(strings.TrimSpace(term), " ", ""))
			if err != nil {
				t.Fatalf("parsing %q in %q: %v", term, field, err)
			}
			if i == 0 {
				total = v
			} else {
				total += v
			}
		}
		out = append(out, strconv.Itoa(total))
	}
	return out
}
