package silk

// The indices a frame carries are selections, not values. Turning them into the gains, filter
// coefficients and pitch lags the synthesis needs is a separate step, and most of it depends on the
// previous frame: gains accumulate, and the spectrum may be interpolated towards the one before.
//
// Adapted from silk/decode_parameters.c, silk/gain_quant.c and silk/NLSF_decode.c.

// Gain quantiser bounds, from silk/define.h and silk/gain_quant.c.
const (
	gainLevels        = 64
	minDeltaGainQuant = -4
	maxDeltaGainQuant = 36
	// gainOffset and gainInvScale place the index on a decibel scale between 2 and 88 dB.
	gainOffset   = (2*128)/6 + 16*128
	gainInvScale = (65536 * (((88 - 2) * 128) / 6)) / (gainLevels - 1)
	// maxGainLogQ7 is 31 in Q7, the largest gain the linear conversion will produce.
	maxGainLogQ7 = 3967
)

// nlsfQuantLevelAdj is the dead zone either side of zero in the residual quantiser, in Q10.
const nlsfQuantLevelAdj = 102 // 0.1 in Q10

// nlsfWQ is the fractional bits the line-spectral weights carry.
const nlsfWQ = 2

// Params is one frame's decoded parameters, ready for synthesis.
type Params struct {
	// GainsQ16 is one linear gain per subframe.
	GainsQ16 [MaxSubframesInFrame]int32
	// LPCQ12 holds the short-term filter for the frame's two halves. The first may be interpolated
	// towards the previous frame's, in which case it differs from the second.
	LPCQ12 [2][]int16
	// NLSFQ15 is the line-spectral representation the coefficients came from, kept because the next
	// frame may interpolate towards it.
	NLSFQ15 []int16
}

// DequantiseGains expands the gain indices into linear gains.
//
// Only the first gain of an independently coded frame is absolute, and even that is floored against
// the previous frame: a gain is not allowed to fall more than sixteen steps at once, which bounds
// how far a lost packet can throw the level.
func DequantiseGains(indices []int, prev *int32, conditional bool, subframes int) [MaxSubframesInFrame]int32 {
	var gains [MaxSubframesInFrame]int32

	for k := range subframes {
		if k == 0 && !conditional {
			*prev = max(int32(indices[k]), *prev-16)
		} else {
			delta := int32(indices[k]) + minDeltaGainQuant
			// Past a threshold the steps double, so a large change costs no more symbols than a
			// small one but lands further.
			threshold := int32(2*maxDeltaGainQuant - gainLevels + int(*prev))
			if delta > threshold {
				*prev += delta<<1 - threshold
			} else {
				*prev += delta
			}
		}
		*prev = limit(*prev, 0, gainLevels-1)

		gains[k] = log2lin(min(smulwb(gainInvScale, *prev)+gainOffset, maxGainLogQ7))
	}
	return gains
}

// residualDequant expands the second-stage line-spectral residuals.
//
// The predictor runs backwards, from the highest coefficient down, because that is the direction the
// quantiser ran; each residual is coded relative to the one above it.
func residualDequant(indices []int, predQ8 []uint8, stepQ16 int32, order int) []int16 {
	out := make([]int16, order)
	var running int32

	for i := order - 1; i >= 0; i-- {
		predQ10 := smulbb(running, int32(int16(predQ8[i]))) >> 8

		running = int32(indices[i]) << 10
		// A dead zone either side of zero, so that a residual of one step is not read as larger than
		// it is.
		switch {
		case running > 0:
			running -= nlsfQuantLevelAdj
		case running < 0:
			running += nlsfQuantLevelAdj
		}
		running = smlawb(predQ10, running, stepQ16)
		out[i] = int16(running)
	}
	return out
}

// laroiaWeights returns how sensitive each line-spectral frequency is to being moved.
//
// A frequency close to its neighbours matters more, because the spectral peak between them is
// sharper. The weights are the reciprocals of the gaps either side, so a narrow gap weighs heavily.
func laroiaWeights(nlsfQ15 []int16, order int) []int16 {
	w := make([]int16, order)
	const scale = 1 << (15 + nlsfWQ)

	inv := func(gap int32) int32 {
		return scale / max(gap, 1)
	}

	lower := inv(int32(nlsfQ15[0]))
	upper := inv(int32(nlsfQ15[1]) - int32(nlsfQ15[0]))
	w[0] = int16(min(lower+upper, 32767))

	for k := 1; k < order-1; k += 2 {
		lower = inv(int32(nlsfQ15[k+1]) - int32(nlsfQ15[k]))
		w[k] = int16(min(lower+upper, 32767))
		upper = inv(int32(nlsfQ15[k+2]) - int32(nlsfQ15[k+1]))
		w[k+1] = int16(min(lower+upper, 32767))
	}

	lower = inv(1<<15 - int32(nlsfQ15[order-1]))
	w[order-1] = int16(min(lower+upper, 32767))
	return w
}

// stabiliseNLSF pushes line-spectral frequencies apart until they are ordered and far enough
// separated to give a stable filter.
//
// Quantisation can leave two frequencies crossed or nearly coincident, which turns into a filter
// that rings or diverges. The fix moves the offending pair apart about their midpoint, which
// disturbs the spectrum least.
func stabiliseNLSF(nlsfQ15 []int16, deltaMinQ15 []int16, order int) {
	const maxLoops = 20

	for range maxLoops {
		// Find the tightest gap, counting the space below the first and above the last.
		minDiff := int32(nlsfQ15[0]) - int32(deltaMinQ15[0])
		worst := 0
		for i := 1; i <= order-1; i++ {
			diff := int32(nlsfQ15[i]) - (int32(nlsfQ15[i-1]) + int32(deltaMinQ15[i]))
			if diff < minDiff {
				minDiff, worst = diff, i
			}
		}
		if diff := 1<<15 - (int32(nlsfQ15[order-1]) + int32(deltaMinQ15[order])); diff < minDiff {
			minDiff, worst = diff, order
		}
		if minDiff >= 0 {
			return
		}

		switch worst {
		case 0:
			nlsfQ15[0] = deltaMinQ15[0]
		case order:
			nlsfQ15[order-1] = int16(1<<15 - int32(deltaMinQ15[order]))
		default:
			// The pair moves apart about its midpoint, but the midpoint itself is confined so that
			// everything below and above still has room for its own minimum spacing.
			lowest := int32(0)
			for k := range worst {
				lowest += int32(deltaMinQ15[k])
			}
			lowest += int32(deltaMinQ15[worst]) >> 1

			highest := int32(1) << 15
			for k := order; k > worst; k-- {
				highest -= int32(deltaMinQ15[k])
			}
			highest -= int32(deltaMinQ15[worst]) >> 1

			centre := limit(rshiftRound(int32(nlsfQ15[worst-1])+int32(nlsfQ15[worst]), 1), lowest, highest)
			nlsfQ15[worst-1] = int16(centre - int32(deltaMinQ15[worst])>>1)
			nlsfQ15[worst] = int16(int32(nlsfQ15[worst-1]) + int32(deltaMinQ15[worst]))
		}
	}

	// Twenty passes without settling means the vector was badly disordered. Sorting and then forcing
	// the spacing is worse spectrally but always terminates.
	sortAscending(nlsfQ15[:order])
	nlsfQ15[0] = max(nlsfQ15[0], deltaMinQ15[0])
	for i := 1; i < order; i++ {
		// The sum is saturated rather than allowed to wrap: an extreme stream can name frequencies
		// whose spacing overflows, and a wrapped value would order them backwards. RFC 8251
		// section 7.
		nlsfQ15[i] = max(nlsfQ15[i], addSat16(nlsfQ15[i-1], deltaMinQ15[i]))
	}
	nlsfQ15[order-1] = min(nlsfQ15[order-1], int16(1<<15-int32(deltaMinQ15[order])))
	for i := order - 2; i >= 0; i-- {
		nlsfQ15[i] = min(nlsfQ15[i], int16(int32(nlsfQ15[i+1])-int32(deltaMinQ15[i+1])))
	}
}

// sortAscending is an insertion sort, which the reference chooses because the input is almost always
// already ordered.
func sortAscending(v []int16) {
	for i := 1; i < len(v); i++ {
		x := v[i]
		j := i - 1
		for j >= 0 && v[j] > x {
			v[j+1] = v[j]
			j--
		}
		v[j+1] = x
	}
}

// DecodeNLSF expands the line-spectral indices into a frequency vector.
func DecodeNLSF(indices []int, cb *nlsfCodebook) []int16 {
	order := cb.order
	nlsf := make([]int16, order)

	// The first stage is a coarse vector held in eight bits per coefficient.
	base := cb.cb1[indices[0]*order:]
	for i := range order {
		nlsf[i] = int16(base[i]) << 7
	}

	_, predQ8 := cb.unpackNLSF(indices[0])
	res := residualDequant(indices[1:], predQ8, cb.stepSize, order)
	weights := laroiaWeights(nlsf, order)

	// A residual is scaled by the inverse root of its weight, so a tightly packed frequency moves
	// less for the same coded value than a loosely packed one.
	for i := range order {
		wQ9 := sqrtApprox(int32(weights[i]) << (18 - nlsfWQ))
		v := int32(nlsf[i]) + (int32(res[i])<<14)/wQ9
		nlsf[i] = int16(limit(v, 0, 32767))
	}

	stabiliseNLSF(nlsf, cb.deltaMinQ, order)
	return nlsf
}

// Pitch lag bounds in milliseconds, from silk/pitch_est_defines.h.
const (
	pitchMinLagMS = 2
	pitchMaxLagMS = 18
)

// ltpOrder is the length of the long-term prediction filter.
const ltpOrder = 5

// contourOffset returns the lag offset for one subframe of one contour.
//
// A frame codes one lag and then a contour: how the lag drifts across the frame's subframes. Pitch
// moves within a frame, and coding the drift from a codebook costs less than coding four lags.
func contourOffset(rateKHz, subframes, subframe, contour int) int {
	if rateKHz == NarrowBand {
		if subframes == SubframesPerFrame {
			return int(cbLagsStage2[subframe][contour])
		}
		return int(cbLagsStage2_10ms[subframe][contour])
	}
	if subframes == SubframesPerFrame {
		return int(cbLagsStage3[subframe][contour])
	}
	return int(cbLagsStage3_10ms[subframe][contour])
}

// DecodePitchLags expands a lag index and contour into one lag per subframe.
func DecodePitchLags(lagIndex, contourIndex, rateKHz, subframes int) []int {
	minLag := pitchMinLagMS * rateKHz
	maxLag := pitchMaxLagMS * rateKHz
	lag := minLag + lagIndex

	lags := make([]int, subframes)
	for k := range subframes {
		lags[k] = lag + contourOffset(rateKHz, subframes, k, contourIndex)
		lags[k] = min(max(lags[k], minLag), maxLag)
	}
	return lags
}

// ltpTap returns one tap of one entry of the long-term gain codebook.
//
// There are three codebooks, coarse to fine. A strongly periodic signal earns the fine one, where
// the extra resolution is worth its extra bits.
func ltpTap(periodicity, entry, tap int) int8 {
	switch periodicity {
	case 0:
		return ltpGainVQ0[entry][tap]
	case 1:
		return ltpGainVQ1[entry][tap]
	default:
		return ltpGainVQ2[entry][tap]
	}
}

// DecodeLTPCoefficients expands the long-term gain indices into filter taps in Q14.
func DecodeLTPCoefficients(periodicity int, indices []int, subframes int) []int16 {
	out := make([]int16, ltpOrder*subframes)
	for k := range subframes {
		for i := range ltpOrder {
			out[k*ltpOrder+i] = int16(ltpTap(periodicity, indices[k], i)) << 7
		}
	}
	return out
}

// LTPScale returns how much of the previous frame's excitation carries into this one, in Q14.
func LTPScale(index int) int16 { return ltpScales[index] }
