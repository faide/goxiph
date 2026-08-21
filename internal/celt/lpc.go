package celt

// Linear prediction is what packet loss concealment extrapolates with: a filter fitted to the last
// of the signal, driven by a repeated stretch of its own excitation. These are the pieces that fit
// and run that filter.
//
// Adapted from celt/celt_lpc.c.

// lpcOrder is the prediction order concealment fits, from celt/celt_lpc.h.
const lpcOrder = 24

// Everything here works in single precision, as the reference does. The fit is ill-conditioned
// enough on real signal that widening the accumulators moves the concealed output audibly, so the
// precision is part of the procedure rather than an implementation detail.

// autocorrelate returns lag+1 autocorrelation values of x, windowing the ends first where a window
// is given.
//
// Tapering the ends stops the fit from reading the buffer's edges as a discontinuity, which would
// put energy at every frequency and flatten the filter.
func autocorrelate(x []float32, window []float32, overlap, lag int) []float32 {
	n := len(x)
	xx := make([]float32, n)
	copy(xx, x)
	for i := range overlap {
		xx[i] = x[i] * window[i]
		xx[n-i-1] = x[n-i-1] * window[i]
	}

	ac := make([]float32, lag+1)
	for l := lag; l >= 0; l-- {
		var d float32
		for i := l; i < n; i++ {
			d += xx[i] * xx[i-l]
		}
		ac[l] = d
	}

	// A flat noise floor on the zero lag. It keeps the fit from becoming arbitrarily sharp on a
	// signal that is nearly periodic, and it is part of the procedure rather than a safeguard: the
	// coefficients differ substantially without it.
	ac[0] += 10
	return ac
}

// levinson fits prediction coefficients to an autocorrelation.
//
// It stops early once the prediction error has fallen far enough that further terms would be fitting
// noise, which is both cheaper and steadier than running the recursion out.
func levinson(ac []float32, order int) []float32 {
	lpc := make([]float32, order)
	if ac[0] == 0 {
		return lpc
	}

	err := ac[0]
	for i := range order {
		// The reflection coefficient that cancels what the filter so far has left at this lag.
		var rr float32
		for j := range i {
			rr += lpc[j] * ac[i-j]
		}
		rr += ac[i+1]
		r := -rr / err

		lpc[i] = r
		for j := range (i + 1) / 2 {
			a, b := lpc[j], lpc[i-1-j]
			lpc[j] = a + r*b
			lpc[i-1-j] = b + r*a
		}

		err -= r * r * err
		// Thirty decibels of prediction gain is as much as is worth having.
		if err < 0.001*ac[0] {
			break
		}
	}
	return lpc
}

// fir runs a finite-impulse-response filter, carrying its memory across calls.
func fir(x, coefs, y []float32, order int, mem []float32) {
	for i := range x {
		sum := x[i]
		for j := range order {
			sum += coefs[j] * mem[j]
		}
		for j := order - 1; j >= 1; j-- {
			mem[j] = mem[j-1]
		}
		mem[0] = x[i]
		y[i] = sum
	}
}

// iir runs the same filter the other way, turning an excitation back into a signal.
func iir(x, coefs, y []float32, order int, mem []float32) {
	for i := range x {
		sum := x[i]
		for j := range order {
			sum -= coefs[j] * mem[j]
		}
		for j := order - 1; j >= 1; j-- {
			mem[j] = mem[j-1]
		}
		mem[0] = sum
		y[i] = sum
	}
}
