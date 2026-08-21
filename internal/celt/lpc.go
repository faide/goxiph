package celt

// Linear prediction is what packet loss concealment extrapolates with: a filter fitted to the last
// of the signal, driven by a repeated stretch of its own excitation. These are the pieces that fit
// and run that filter.
//
// Adapted from celt/celt_lpc.c.

// lpcOrder is the prediction order concealment fits, from celt/celt_lpc.h.
const lpcOrder = 24

// autocorrelate returns lag+1 autocorrelation values of x, windowing the ends first where a window
// is given.
//
// Tapering the ends stops the fit from reading the buffer's edges as a discontinuity, which would
// put energy at every frequency and flatten the filter.
func autocorrelate(x []float32, window []float32, overlap, lag int) []float64 {
	n := len(x)
	xx := make([]float64, n)
	for i, v := range x {
		xx[i] = float64(v)
	}
	for i := range overlap {
		xx[i] = float64(x[i]) * float64(window[i])
		xx[n-i-1] = float64(x[n-i-1]) * float64(window[i])
	}

	ac := make([]float64, lag+1)
	for l := lag; l >= 0; l-- {
		var d float64
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
func levinson(ac []float64, order int) []float32 {
	lpc := make([]float32, order)
	if ac[0] == 0 {
		return lpc
	}

	work := make([]float64, order)
	err := ac[0]
	for i := range order {
		// The reflection coefficient that cancels what the filter so far has left at this lag.
		rr := 0.0
		for j := range i {
			rr += work[j] * ac[i-j]
		}
		rr += ac[i+1]
		r := -rr / err

		work[i] = r
		for j := range (i + 1) / 2 {
			a, b := work[j], work[i-1-j]
			work[j] = a + r*b
			work[i-1-j] = b + r*a
		}

		err -= r * r * err
		// Thirty decibels of prediction gain is as much as is worth having.
		if err < 0.001*ac[0] {
			break
		}
	}

	for i := range order {
		lpc[i] = float32(work[i])
	}
	return lpc
}

// fir runs a finite-impulse-response filter, carrying its memory across calls.
func fir(x, coefs, y []float32, order int, mem []float32) {
	for i := range x {
		sum := float64(x[i])
		for j := range order {
			sum += float64(coefs[j]) * float64(mem[j])
		}
		for j := order - 1; j >= 1; j-- {
			mem[j] = mem[j-1]
		}
		mem[0] = x[i]
		y[i] = float32(sum)
	}
}

// iir runs the same filter the other way, turning an excitation back into a signal.
func iir(x, coefs, y []float32, order int, mem []float32) {
	for i := range x {
		sum := float64(x[i])
		for j := range order {
			sum -= float64(coefs[j]) * float64(mem[j])
		}
		for j := order - 1; j >= 1; j-- {
			mem[j] = mem[j-1]
		}
		mem[0] = float32(sum)
		y[i] = float32(sum)
	}
}
