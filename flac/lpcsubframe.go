package flac

import (
	"math"

	"github.com/faide/goxiph/internal/lpc"
)

// maxCoefficientPrecision is the widest coefficient the format can express: the field stores the
// precision minus one in four bits, and the all-ones code is forbidden.
const maxCoefficientPrecision = 15

// lpcPlan is a candidate linear-predictor subframe.
type lpcPlan struct {
	order     int
	precision uint
	shift     int
	coeffs    [maxLPCOrder]int32
	partOrder int
	plans     []partitionPlan
	cost      int
	valid     bool
}

// lpcState is the scratch a linear-predictor search needs, allocated once per stream.
type lpcState struct {
	windowed []float64
	window   []float64
	autoc    []float64
	coeffs   [][]float64
	errs     []float64
	maxOrder int
}

func newLPCState(blockSize, maxOrder int) *lpcState {
	s := &lpcState{
		windowed: make([]float64, blockSize),
		window:   make([]float64, blockSize),
		autoc:    make([]float64, maxOrder+1),
		errs:     make([]float64, maxOrder),
		maxOrder: maxOrder,
	}
	lpc.TukeyWindow(s.window, 0.5)
	s.coeffs = make([][]float64, maxOrder)
	for i := range s.coeffs {
		s.coeffs[i] = make([]float64, maxOrder)
	}
	return s
}

// bestLPCPredictor searches for the linear predictor with the smallest coded size.
//
// Returns an invalid plan when no predictor is worth using, which happens for signals with no
// structure to model and for blocks too short to carry the warm-up samples.
func (e *Encoder) bestLPCPredictor(samples []int32, depth uint) lpcPlan {
	var best lpcPlan
	s := e.lpc
	if s == nil || s.maxOrder == 0 {
		return best
	}

	n := len(samples)
	maxOrder := min(s.maxOrder, n-1)
	if maxOrder < 1 {
		return best
	}

	// Autocorrelation treats the block as periodic, so the ends are tapered before estimating. The
	// residual itself is computed from the untouched samples.
	//
	// The window is built for the configured block size and rebuilt only for a short final frame,
	// so the taper always matches the block it is applied to.
	if len(s.window) != n {
		s.window = make([]float64, n)
		lpc.TukeyWindow(s.window, 0.5)
	}
	for i := range n {
		s.windowed[i] = float64(samples[i]) * s.window[i]
	}

	lpc.Autocorrelate(s.windowed[:n], maxOrder, s.autoc)
	if s.autoc[0] == 0 {
		return best // silence, which the constant form already handles
	}

	solved := lpc.Levinson(s.autoc, maxOrder, s.coeffs, s.errs)
	if solved < 1 {
		return best
	}

	// Pick a candidate order from the residual energy rather than trying all of them: each exact
	// evaluation costs a full pass over the block, and the estimate is close enough to bracket.
	estimateOrder := 1
	bestEstimate := math.Inf(1)
	for o := 1; o <= solved; o++ {
		bits := estimateResidualBits(s.errs[o-1], n) + float64(o)*float64(maxCoefficientPrecision+depth)
		if bits < bestEstimate {
			bestEstimate, estimateOrder = bits, o
		}
	}

	for _, o := range candidateOrders(estimateOrder, solved) {
		if p := e.evaluateLPC(samples, depth, o, s.coeffs[o-1]); p.valid && (!best.valid || p.cost < best.cost) {
			best = p
		}
	}
	return best
}

// candidateOrders returns the orders worth evaluating exactly around an estimate.
func candidateOrders(estimate, maxOrder int) []int {
	out := make([]int, 0, 3)
	for _, o := range [...]int{estimate - 1, estimate, estimate + 1} {
		if o >= 1 && o <= maxOrder {
			out = append(out, o)
		}
	}
	return out
}

// estimateResidualBits approximates the coded size of a residual from its energy.
//
// A Rice code spends roughly log2 of the mean magnitude per sample, and energy is the square of
// magnitude, hence the half.
func estimateResidualBits(energy float64, n int) float64 {
	if energy <= 0 || n == 0 {
		return 0
	}
	mean := energy / float64(n)
	if mean <= 1 {
		return 0
	}
	return float64(n) * 0.5 * math.Log2(mean)
}

// evaluateLPC quantises one predictor and measures its exact coded size.
func (e *Encoder) evaluateLPC(samples []int32, depth uint, order int, coef []float64) lpcPlan {
	var p lpcPlan
	if order >= len(samples) {
		return p
	}

	coeffs, shift, ok := quantizeCoefficients(coef[:order], maxCoefficientPrecision)
	if !ok {
		return p
	}
	p.order, p.precision, p.shift = order, maxCoefficientPrecision, shift
	copy(p.coeffs[:order], coeffs)

	// The residual must be computed with the same integer arithmetic the decoder uses, or the two
	// disagree and the stream stops being lossless.
	res := e.residual[:len(samples)]
	if !lpcResidual(samples, p.coeffs[:order], shift, res) {
		return p
	}

	partOrder, plans, cost := e.bestPartitioning(res[order:], len(samples), order)
	p.partOrder, p.plans = partOrder, plans
	p.cost = cost + int(depth)*order + 4 + 5 + int(maxCoefficientPrecision)*order
	p.valid = true
	return p
}

// lpcResidual fills dst[order:] with the prediction error, reporting whether every value stayed in
// the range the format permits.
//
// RFC 9639 section 9.2.7.3 requires residuals to fit a 32-bit signed integer excluding its most
// negative value, so a predictor that overflows is rejected rather than written.
func lpcResidual(samples []int32, coeffs []int32, shift int, dst []int32) bool {
	order := len(coeffs)
	for i := order; i < len(samples); i++ {
		var pred int64
		for j := range order {
			pred += int64(coeffs[j]) * int64(samples[i-1-j])
		}
		r := int64(samples[i]) - (pred >> uint(shift))
		if r > math.MaxInt32 || r <= math.MinInt32 {
			return false
		}
		dst[i] = int32(r)
	}
	return true
}

// quantizeCoefficients converts coef coefficients to the integer form the bitstream carries.
//
// The shift is chosen so the largest coefficient fills the available precision, and the rounding
// error is carried into the next coefficient so it cannot accumulate in one direction.
func quantizeCoefficients(coef []float64, precision uint) ([]int32, int, bool) {
	cmax := 0.0
	for _, c := range coef {
		if math.IsNaN(c) || math.IsInf(c, 0) {
			return nil, 0, false
		}
		cmax = math.Max(cmax, math.Abs(c))
	}
	if cmax <= 0 {
		return nil, 0, false
	}

	// The shift places the most significant bit of the largest coefficient at the top of the field.
	_, exp := math.Frexp(cmax)
	shift := int(precision) - exp - 1
	if shift > 15 {
		shift = 15 // the field is a five-bit signed value, and must not be negative
	}
	if shift < 0 {
		return nil, 0, false
	}

	limit := int64(1)<<(precision-1) - 1
	lower := -limit - 1

	out := make([]int32, len(coef))
	var carry float64
	scale := math.Ldexp(1, shift)
	for i, c := range coef {
		carry += c * scale
		q := int64(math.Round(carry))
		q = min(max(q, lower), limit)
		carry -= float64(q)
		out[i] = int32(q)
	}
	return out, shift, true
}
