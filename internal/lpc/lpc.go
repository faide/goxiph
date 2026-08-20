// Package lpc computes linear prediction coefficients.
//
// FLAC uses these directly, and the SILK half of Opus needs the same machinery, so the numerics live
// outside both.
package lpc

import "math"

// Autocorrelate fills out[0:lags+1] with the autocorrelation of x at each lag.
//
// The sum is unnormalised, which is all Levinson needs: it uses ratios of these values and any
// common scale cancels.
func Autocorrelate(x []float64, lags int, out []float64) {
	for lag := 0; lag <= lags; lag++ {
		var sum float64
		for i := lag; i < len(x); i++ {
			sum += x[i] * x[i-lag]
		}
		out[lag] = sum
	}
}

// Levinson solves the normal equations for every order up to maxOrder.
//
// coeffs[i] holds the order i+1 predictor, where coeffs[i][j] multiplies the sample j+1 places back:
// the same ordering FLAC stores. errs[i] is that order's residual energy, which falls monotonically
// and is what an order search compares.
//
// It returns the highest order that stayed numerically sound, which can be below maxOrder when the
// signal is short or degenerate.
func Levinson(autoc []float64, maxOrder int, coeffs [][]float64, errs []float64) int {
	if maxOrder <= 0 || autoc[0] == 0 {
		return 0
	}

	a := make([]float64, maxOrder+1)
	next := make([]float64, maxOrder+1)
	err := autoc[0]

	for i := 1; i <= maxOrder; i++ {
		acc := autoc[i]
		for j := 1; j < i; j++ {
			acc -= a[j] * autoc[i-j]
		}
		if err == 0 {
			return i - 1
		}
		k := acc / err

		// A reflection coefficient outside the unit circle means the recursion has lost accuracy
		// and the predictor would be unstable.
		if math.IsNaN(k) || math.IsInf(k, 0) || math.Abs(k) >= 1 {
			return i - 1
		}

		copy(next, a)
		next[i] = k
		for j := 1; j < i; j++ {
			next[j] = a[j] - k*a[i-j]
		}
		a, next = next, a

		err *= 1 - k*k
		if err <= 0 {
			return i - 1
		}

		copy(coeffs[i-1], a[1:i+1])
		errs[i-1] = err
	}
	return maxOrder
}

// TukeyWindow fills w with a Tukey window of the given taper fraction.
//
// Autocorrelation assumes the signal is periodic, which a raw block is not; tapering the ends keeps
// the discontinuity between the last sample and the first from dominating the estimate. A taper of
// 0.5 leaves the middle half untouched.
func TukeyWindow(w []float64, taper float64) {
	n := len(w)
	if n == 0 {
		return
	}
	if taper <= 0 {
		for i := range w {
			w[i] = 1
		}
		return
	}
	taper = math.Min(taper, 1)

	edge := int(taper * float64(n-1) / 2)
	for i := range w {
		switch {
		case i < edge:
			w[i] = 0.5 * (1 + math.Cos(math.Pi*(2*float64(i)/(taper*float64(n-1))-1)))
		case i > n-1-edge:
			w[i] = 0.5 * (1 + math.Cos(math.Pi*(2*float64(n-1-i)/(taper*float64(n-1))-1)))
		default:
			w[i] = 1
		}
	}
}
