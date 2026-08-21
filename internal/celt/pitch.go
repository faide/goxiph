package celt

// Concealment repeats the signal at its own pitch, so it has to find that pitch first. The search
// runs on a heavily decimated copy: pitch is a low-frequency property, and comparing every sample
// would cost far more for no better an answer.
//
// Adapted from celt/pitch.c.

// pitchDownsample halves the rate and flattens the spectrum, so that the correlation that follows
// measures periodicity rather than whichever band happens to be loudest.
func pitchDownsample(channels [][]float32, out []float32, length int) {
	half := length >> 1
	for i := 1; i < half; i++ {
		out[i] = 0.5 * (0.5*(channels[0][2*i-1]+channels[0][2*i+1]) + channels[0][2*i])
	}
	out[0] = 0.5 * (0.5*channels[0][1] + channels[0][0])

	if len(channels) == 2 {
		for i := 1; i < half; i++ {
			out[i] += 0.5 * (0.5*(channels[1][2*i-1]+channels[1][2*i+1]) + channels[1][2*i])
		}
		out[0] += 0.5 * (0.5*channels[1][1] + channels[1][0])
	}

	ac := autocorrelate(out[:half], nil, 0, 4)
	// A noise floor forty decibels down, and a lag window, so the fit cannot become sharp enough to
	// ring on a signal that is nearly periodic but not quite.
	ac[0] *= 1.0001
	for i := 1; i <= 4; i++ {
		ac[i] -= ac[i] * (0.008 * float64(i)) * (0.008 * float64(i))
	}

	lpc := levinson(ac, 4)
	// Widening the filter's resonances leaves the flattening gentle.
	tmp := float32(1)
	for i := range 4 {
		tmp *= 0.9
		lpc[i] *= tmp
	}

	mem := make([]float32, 5)
	fir(out[:half], lpc, out[:half], 4, mem)

	// A final gentle tilt, which is what removes the remaining spectral slope.
	mem[0] = 0
	lpc[0] = 0.8
	fir(out[:half], lpc, out[:half], 1, mem)
}

// pitchSearch returns the lag at which the signal best repeats.
//
// It looks twice: once on a four-times decimated copy to find the neighbourhood, then once at half
// that decimation near the two best candidates. Searching the whole range at the finer rate would
// cost four times as much to reach the same lag.
func pitchSearch(xLP, y []float32, length, maxPitch int) int {
	lag := length + maxPitch

	xLP4 := make([]float32, length>>2)
	yLP4 := make([]float32, lag>>2)
	for j := range xLP4 {
		xLP4[j] = xLP[2*j]
	}
	for j := range yLP4 {
		yLP4[j] = y[2*j]
	}

	xcorr := make([]float64, maxPitch>>1)
	coarse := xcorr[:maxPitch>>2]
	for i := range coarse {
		var sum float64
		for j := range xLP4 {
			sum += float64(xLP4[j]) * float64(yLP4[i+j])
		}
		coarse[i] = max(-1, sum)
	}
	best := findBestPitch(coarse, yLP4, length>>2, maxPitch>>2)

	for i := range xcorr {
		xcorr[i] = 0
		// Only the neighbourhoods of the two coarse candidates are worth refining.
		if abs(i-2*best[0]) > 2 && abs(i-2*best[1]) > 2 {
			continue
		}
		var sum float64
		for j := range length >> 1 {
			sum += float64(xLP[j]) * float64(y[i+j])
		}
		xcorr[i] = max(-1, sum)
	}
	best = findBestPitch(xcorr, y, length>>1, maxPitch>>1)

	// A half-step refinement from the shape of the peak, which costs nothing and halves the error.
	offset := 0
	if best[0] > 0 && best[0] < (maxPitch>>1)-1 {
		a, b, c := xcorr[best[0]-1], xcorr[best[0]], xcorr[best[0]+1]
		switch {
		case c-a > 0.7*(b-a):
			offset = 1
		case a-c > 0.7*(b-c):
			offset = -1
		}
	}
	return 2*best[0] - offset
}

// findBestPitch returns the two lags with the strongest normalised correlation.
//
// Normalising by the energy at each lag is what stops a loud stretch of signal from winning on
// amplitude alone; the running update of that energy is why the loop is written as it is.
func findBestPitch(xcorr []float64, y []float32, length, maxPitch int) [2]int {
	var bestNum [2]float64
	var bestDen [2]float64
	bestNum[0], bestNum[1] = -1, -1
	best := [2]int{0, 1}

	syy := 1.0
	for j := range length {
		syy += float64(y[j]) * float64(y[j])
	}

	for i := range maxPitch {
		if xcorr[i] > 0 {
			num := xcorr[i] * xcorr[i]
			if num*bestDen[1] > bestNum[1]*syy {
				if num*bestDen[0] > bestNum[0]*syy {
					bestNum[1], bestDen[1], best[1] = bestNum[0], bestDen[0], best[0]
					bestNum[0], bestDen[0], best[0] = num, syy, i
				} else {
					bestNum[1], bestDen[1], best[1] = num, syy, i
				}
			}
		}
		syy += float64(y[i+length])*float64(y[i+length]) - float64(y[i])*float64(y[i])
		syy = max(1, syy)
	}
	return best
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
