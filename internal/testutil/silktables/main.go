// Command silktables transcribes the SILK constant tables from the RFC 6716 reference into Go.
//
// SILK carries about twelve hundred lines of codebooks and distributions. Typing them is a source of
// silent faults: a wrong entry in a vector quantiser codebook produces audio that is merely wrong
// rather than plainly broken. Reading them from the reference removes that, and the conformance
// test in the silk package reads the reference again to check what this produced.
package main

import (
	"flag"
	"fmt"
	"go/format"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// table names the reference array to take and what to call it here.
type table struct {
	ref  string
	name string
	doc  string
}

var tables = []table{
	// Frame type and gains.
	{"silk_type_offset_VAD_iCDF", "typeOffsetVADICDF", "signal type and quantiser offset, in an active frame"},
	{"silk_type_offset_no_VAD_iCDF", "typeOffsetNoVADICDF", "the same, where the frame is inactive"},
	{"silk_gain_iCDF", "gainICDF", "the first subframe's gain, by signal type"},
	{"silk_delta_gain_iCDF", "deltaGainICDF", "a gain coded as a change from the one before"},

	// Uniform distributions, used where a value has no useful prior.
	{"silk_uniform3_iCDF", "uniform3ICDF", ""},
	{"silk_uniform4_iCDF", "uniform4ICDF", ""},
	{"silk_uniform5_iCDF", "uniform5ICDF", ""},
	{"silk_uniform6_iCDF", "uniform6ICDF", ""},
	{"silk_uniform8_iCDF", "uniform8ICDF", ""},

	// Line spectral frequencies.
	{"silk_NLSF_EXT_iCDF", "nlsfExtensionICDF", "the tail of a residual that saturated its codebook"},
	{"silk_NLSF_interpolation_factor_iCDF", "nlsfInterpolationICDF", "how far to interpolate towards the previous frame's spectrum"},

	// Pitch.
	{"silk_pitch_delta_iCDF", "pitchDeltaICDF", "a pitch lag coded as a change from the previous frame's"},
	{"silk_pitch_lag_iCDF", "pitchLagICDF", "the high bits of an absolutely coded pitch lag"},
	{"silk_pitch_contour_iCDF", "pitchContourICDFTable", "the per-subframe deviation from the frame's lag, wideband"},
	{"silk_pitch_contour_NB_iCDF", "pitchContourNBICDF", "the same, narrowband"},
	{"silk_pitch_contour_10_ms_iCDF", "pitchContour10msICDF", "the same, in a ten millisecond frame"},
	{"silk_pitch_contour_10_ms_NB_iCDF", "pitchContour10msNBICDF", ""},
	{"silk_CB_lags_stage2", "cbLagsStage2", "the contour codebook: per-subframe lag offsets"},
	{"silk_CB_lags_stage2_10_ms", "cbLagsStage2_10ms", ""},
	{"silk_CB_lags_stage3", "cbLagsStage3", ""},
	{"silk_CB_lags_stage3_10_ms", "cbLagsStage3_10ms", ""},

	// Long-term prediction.
	{"silk_LTP_per_index_iCDF", "ltpPeriodicityICDF", "which of the three long-term codebooks the frame uses"},
	{"silk_LTP_gain_iCDF_0", "ltpGainICDF0", ""},
	{"silk_LTP_gain_iCDF_1", "ltpGainICDF1", ""},
	{"silk_LTP_gain_iCDF_2", "ltpGainICDF2", ""},
	{"silk_LTP_gain_vq_0", "ltpGainVQ0", ""},
	{"silk_LTP_gain_vq_1", "ltpGainVQ1", ""},
	{"silk_LTP_gain_vq_2", "ltpGainVQ2", ""},
	{"silk_LTP_vq_sizes", "ltpVQSizes", ""},
	{"silk_LTPscale_iCDF", "ltpScaleICDF", "how much of the previous frame's excitation carries in"},
	{"silk_LTPScales_table_Q14", "ltpScales", ""},

	// Excitation.
	{"silk_rate_levels_iCDF", "rateLevelICDF", "which pulse-count distribution the frame uses"},
	{"silk_pulses_per_block_iCDF", "pulseCountICDF", "the number of pulses in a sixteen-sample block"},
	{"silk_max_pulses_table", "maxPulsesTable", ""},
	{"silk_shell_code_table0", "shellCodeTable0", "how a block's pulses split between its two halves"},
	{"silk_shell_code_table1", "shellCodeTable1", ""},
	{"silk_shell_code_table2", "shellCodeTable2", ""},
	{"silk_shell_code_table3", "shellCodeTable3", ""},
	{"silk_shell_code_table_offsets", "shellCodeOffsets", ""},
	{"silk_lsb_iCDF", "lsbICDF", ""},
	{"silk_sign_iCDF", "signICDF", "an excitation sign, by signal type, quantiser offset and pulse count"},

	// Stereo.
	{"silk_stereo_pred_quant_Q13", "stereoPredictionLevels", ""},
	{"silk_stereo_pred_joint_iCDF", "stereoPredictionJointICDF", ""},
	{"silk_stereo_only_code_mid_iCDF", "stereoMidOnlyICDF", ""},

	// Per-frame flags.
	{"silk_LBRR_flags_2_iCDF", "lbrrFlags2ICDF", "which of two redundant frames a packet carries"},
	{"silk_LBRR_flags_3_iCDF", "lbrrFlags3ICDF", "the same, across three frames"},

	// The two line-spectral codebooks, narrow and wide band.
	{"silk_NLSF_CB1_NB_MB_Q8", "nlsfCB1NBMB", ""},
	{"silk_NLSF_CB1_iCDF_NB_MB", "nlsfCB1ICDFNBMB", ""},
	{"silk_NLSF_PRED_NB_MB_Q8", "nlsfPredNBMB", ""},
	{"silk_NLSF_CB2_SELECT_NB_MB", "nlsfCB2SelectNBMB", ""},
	{"silk_NLSF_CB2_iCDF_NB_MB", "nlsfCB2ICDFNBMB", ""},
	{"silk_NLSF_DELTA_MIN_NB_MB_Q15", "nlsfDeltaMinNBMB", ""},
	{"silk_NLSF_CB1_WB_Q8", "nlsfCB1WB", ""},
	{"silk_NLSF_CB1_iCDF_WB", "nlsfCB1ICDFWB", ""},
	{"silk_NLSF_PRED_WB_Q8", "nlsfPredWB", ""},
	{"silk_NLSF_CB2_SELECT_WB", "nlsfCB2SelectWB", ""},
	{"silk_NLSF_CB2_iCDF_WB", "nlsfCB2ICDFWB", ""},
	{"silk_NLSF_DELTA_MIN_WB_Q15", "nlsfDeltaMinWB", ""},
}

// goType maps a reference element type onto the Go type that holds it without widening.
var goType = map[string]string{
	"opus_uint8": "uint8",
	"opus_int8":  "int8",
	"opus_int16": "int16",
	"opus_int32": "int32",
}

var (
	declRe   = regexp.MustCompile(`(?s)const\s+(opus_u?int\d+)\s+(\w+)\s*((?:\[[^\]]*\]\s*)+)=\s*\{(.*?)\};`)
	dimRe    = regexp.MustCompile(`\[([^\]]*)\]`)
	numRe    = regexp.MustCompile(`-?\d+`)
	defineRe = regexp.MustCompile(`(?m)^#define\s+(\w+)\s+\(?\s*(-?\d+)\s*\)?\s*$`)
)

func main() {
	src := flag.String("src", ".specs/opus-rfc6716/silk", "reference source directory")
	out := flag.String("out", "internal/silk/tables.go", "generated file")
	flag.Parse()

	if err := run(*src, *out); err != nil {
		fmt.Fprintln(os.Stderr, "silktables:", err)
		os.Exit(1)
	}
}

func run(src, out string) error {
	files, err := filepath.Glob(filepath.Join(src, "*.c"))
	if err != nil {
		return err
	}
	headers, err := filepath.Glob(filepath.Join(src, "*.h"))
	if err != nil {
		return err
	}

	// Dimensions are written as macros, so those have to be resolved before a size means anything.
	consts := map[string]int{}
	for _, f := range append(files, headers...) {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		for _, m := range defineRe.FindAllStringSubmatch(stripComments(string(raw)), -1) {
			v, err := strconv.Atoi(m[2])
			if err == nil {
				consts[m[1]] = v
			}
		}
	}

	found := map[string]string{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		body := stripComments(string(raw))
		for _, m := range declRe.FindAllStringSubmatch(body, -1) {
			found[m[2]] = m[1] + "\x00" + m[3] + "\x00" + m[4]
		}
	}

	var b strings.Builder
	b.WriteString("// Code generated by internal/testutil/silktables. DO NOT EDIT.\n\n")
	b.WriteString("package silk\n\n")
	b.WriteString("// The constant tables of RFC 6716 section 4.2, taken from the reference\n")
	b.WriteString("// implementation it embeds. Regenerate with `mise run silk:tables`.\n\n")

	var missing []string
	for _, t := range tables {
		raw, ok := found[t.ref]
		if !ok {
			missing = append(missing, t.ref)
			continue
		}
		parts := strings.SplitN(raw, "\x00", 3)
		elem, ok := goType[parts[0]]
		if !ok {
			return fmt.Errorf("%s: unhandled element type %s", t.ref, parts[0])
		}

		var dims []int
		for _, d := range dimRe.FindAllStringSubmatch(parts[1], -1) {
			n, err := resolve(strings.TrimSpace(d[1]), consts)
			if err != nil {
				return fmt.Errorf("%s: %w", t.ref, err)
			}
			dims = append(dims, n)
		}
		values := numRe.FindAllString(parts[2], -1)

		total := 1
		for _, d := range dims {
			total *= d
		}
		if len(values) != total {
			return fmt.Errorf("%s: %d values for dimensions %v", t.ref, len(values), dims)
		}

		if t.doc != "" {
			fmt.Fprintf(&b, "// %s is %s.\n", t.name, t.doc)
		}
		fmt.Fprintf(&b, "// Transcribed from %s of the reference implementation.\n", t.ref)
		writeArray(&b, t.name, elem, dims, values)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("not found in the reference: %s", strings.Join(missing, ", "))
	}

	formatted, err := format.Source([]byte(b.String()))
	if err != nil {
		return fmt.Errorf("formatting: %w", err)
	}
	if err := os.WriteFile(out, formatted, 0o644); err != nil {
		return err
	}
	fmt.Printf("silktables: wrote %d tables to %s\n", len(tables), out)
	return nil
}

func writeArray(b *strings.Builder, name, elem string, dims []int, values []string) {
	switch len(dims) {
	case 1:
		fmt.Fprintf(b, "var %s = [%d]%s{", name, dims[0], elem)
		for i, v := range values {
			if i%16 == 0 {
				b.WriteString("\n\t")
			}
			fmt.Fprintf(b, "%s, ", v)
		}
		b.WriteString("\n}\n\n")
	case 2:
		fmt.Fprintf(b, "var %s = [%d][%d]%s{\n", name, dims[0], dims[1], elem)
		for i := range dims[0] {
			b.WriteString("\t{")
			for j := range dims[1] {
				fmt.Fprintf(b, "%s, ", values[i*dims[1]+j])
			}
			b.WriteString("},\n")
		}
		b.WriteString("}\n\n")
	default:
		panic("unhandled dimension count")
	}
}

// resolve turns a dimension expression into a number. Dimensions in the reference are macros and
// simple arithmetic on them, so that is what this handles; anything else is a signal to look at the
// declaration rather than guess at it.
func resolve(expr string, consts map[string]int) (int, error) {
	expr = strings.TrimSpace(expr)
	for strings.HasPrefix(expr, "(") && strings.HasSuffix(expr, ")") && balanced(expr[1:len(expr)-1]) {
		expr = strings.TrimSpace(expr[1 : len(expr)-1])
	}
	if n, err := strconv.Atoi(expr); err == nil {
		return n, nil
	}
	if n, ok := consts[expr]; ok {
		return n, nil
	}

	// Split on the lowest-precedence operator outside any parentheses, so both halves resolve on
	// their own. Shifts bind loosest, then addition, then multiplication.
	for _, ops := range []string{"<>", "+-", "*/"} {
		i := lastTopLevel(expr, ops)
		if i <= 0 {
			continue
		}
		op := expr[i]
		left := expr[:i]
		if op == '<' || op == '>' {
			// The operator is doubled, and lastTopLevel found its second character.
			if i < 1 || expr[i-1] != op {
				continue
			}
			left = expr[:i-1]
		}

		l, err := resolve(left, consts)
		if err != nil {
			continue
		}
		r, err := resolve(expr[i+1:], consts)
		if err != nil {
			continue
		}
		switch op {
		case '<':
			return l << uint(r), nil
		case '>':
			return l >> uint(r), nil
		case '+':
			return l + r, nil
		case '-':
			return l - r, nil
		case '*':
			return l * r, nil
		case '/':
			return l / r, nil
		}
	}
	return 0, fmt.Errorf("cannot resolve dimension %q", expr)
}

// lastTopLevel finds the rightmost of the given operators outside any parentheses.
func lastTopLevel(expr, ops string) int {
	depth := 0
	for i := len(expr) - 1; i >= 0; i-- {
		switch expr[i] {
		case ')':
			depth++
		case '(':
			depth--
		default:
			if depth == 0 && strings.ContainsRune(ops, rune(expr[i])) {
				return i
			}
		}
	}
	return -1
}

func balanced(s string) bool {
	depth := 0
	for _, c := range s {
		switch c {
		case '(':
			depth++
		case ')':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0
}

func stripComments(s string) string {
	s = regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(s, "")
	return regexp.MustCompile(`(?m)//.*$`).ReplaceAllString(s, "")
}
