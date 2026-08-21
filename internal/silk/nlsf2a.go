package silk

import "math/bits"

// Line spectral frequencies describe the short-term filter by where its response peaks, which is a
// representation that survives quantisation and interpolation without the filter going unstable.
// Synthesis needs the filter itself, so the frequencies have to be turned back into coefficients.
//
// The conversion builds two polynomials whose roots are the frequencies, one for the symmetric part
// of the filter and one for the antisymmetric, and adds them.
//
// Adapted from silk/NLSF2A.c and silk/LPC_inv_pred_gain.c.

// nlsf2aQA is the fractional precision the polynomial construction works at.
const nlsf2aQA = 16

// predGainQA is the precision the stability check works at, which is finer.
const predGainQA = 24

// aLimit bounds a reflection coefficient before the filter counts as unstable, 0.99975 in Q24.
const aLimit = 16773022

// maxStabiliseIterations bounds the widening applied to an unstable filter.
const maxStabiliseIterations = 16

// minInversePredGainQ30 is one over the largest prediction gain allowed, in Q30. A filter above that
// is too resonant to be an honest reading of the signal.
const minInversePredGainQ30 = 107374 // 1/1e4 in Q30

// The order the frequencies are visited in when building the polynomials.
//
// It is not ascending. The reference found this ordering to keep the intermediate products smaller,
// which matters because they are held in a fixed number of bits.
var (
	nlsf2aOrdering16 = [16]uint8{0, 15, 8, 7, 4, 11, 12, 3, 2, 13, 10, 5, 6, 9, 14, 1}
	nlsf2aOrdering10 = [10]uint8{0, 9, 6, 3, 4, 5, 8, 1, 2, 7}
)

// findPoly builds one of the two polynomials from alternate frequencies, by convolution.
func findPoly(cosLSF []int32, dd int) []int32 {
	out := make([]int32, dd+1)
	out[0] = 1 << nlsf2aQA
	out[1] = -cosLSF[0]

	for k := 1; k < dd; k++ {
		f := cosLSF[2*k]
		out[k+1] = out[k-1]<<1 - int32(rshiftRound64(int64(f)*int64(out[k]), nlsf2aQA))
		for n := k; n > 1; n-- {
			out[n] += out[n-2] - int32(rshiftRound64(int64(f)*int64(out[n-1]), nlsf2aQA))
		}
		out[1] -= f
	}
	return out
}

// bwExpand32 widens a filter's resonances by pulling its roots towards the origin.
//
// Each coefficient is scaled by a chirp factor raised to its index, which flattens the response
// without moving where its peaks are. It is what an unstable filter is fixed with.
func bwExpand32(ar []int32, chirpQ16 int32) {
	chirpMinusOne := chirpQ16 - 65536
	for i := 0; i < len(ar)-1; i++ {
		ar[i] = smulww(chirpQ16, ar[i])
		chirpQ16 += rshiftRound(chirpQ16*chirpMinusOne, 16)
	}
	ar[len(ar)-1] = smulww(chirpQ16, ar[len(ar)-1])
}

// inversePredGain returns one over the filter's prediction gain, in Q30, or zero if it is unstable.
//
// It runs the Levinson recursion backwards, recovering a reflection coefficient at each step. A
// reflection coefficient at or beyond one means a pole outside the unit circle, so the check falls
// out of the recursion rather than needing a root finder.
func inversePredGain(aQ12 []int16) int32 {
	order := len(aQ12)
	var work [2][]int32
	work[0] = make([]int32, order)
	work[1] = make([]int32, order)

	current := work[order&1]
	dcResponse := int32(0)
	for k := range order {
		dcResponse += int32(aQ12[k])
		current[k] = int32(aQ12[k]) << (predGainQA - 12)
	}
	// A filter whose coefficients sum past one has a pole at direct current and cannot be stable.
	if dcResponse >= 4096 {
		return 0
	}

	invGainQ30 := int32(1) << 30
	for k := order - 1; k > 0; k-- {
		if current[k] > aLimit || current[k] < -aLimit {
			return 0
		}
		rcQ31 := -(current[k] << (31 - predGainQA))
		rcMult1Q30 := int32(1)<<30 - smmul(rcQ31, rcQ31)

		mult2Q := uint(32 - bits.LeadingZeros32(uint32(abs32(rcMult1Q30))))
		rcMult2 := inverse32VarQ(rcMult1Q30, mult2Q+30)

		invGainQ30 = smmul(invGainQ30, rcMult1Q30) << 2

		previous := current
		current = work[k&1]
		for n := range k {
			tmp := previous[n] - mulFracQ(previous[k-n-1], rcQ31, 31)
			current[n] = mulFracQ(tmp, rcMult2, mult2Q)
		}
	}

	if current[0] > aLimit || current[0] < -aLimit {
		return 0
	}
	rcQ31 := -(current[0] << (31 - predGainQA))
	rcMult1Q30 := int32(1)<<30 - smmul(rcQ31, rcQ31)
	return smmul(invGainQ30, rcMult1Q30) << 2
}

// NLSFToLPC converts a line-spectral vector into short-term filter coefficients in Q12.
func NLSFToLPC(nlsfQ15 []int16, order int) []int16 {
	ordering := nlsf2aOrdering10[:]
	if order == 16 {
		ordering = nlsf2aOrdering16[:]
	}

	// Each frequency becomes twice its cosine, read from a table and interpolated between entries.
	cosLSF := make([]int32, order)
	for k := range order {
		f := int32(nlsfQ15[k]) >> (15 - 7)
		frac := int32(nlsfQ15[k]) - f<<(15-7)
		cosVal := int32(lsfCosTable[f])
		delta := int32(lsfCosTable[f+1]) - cosVal
		cosLSF[ordering[k]] = rshiftRound(cosVal<<8+delta*frac, 20-nlsf2aQA)
	}

	dd := order >> 1
	p := findPoly(cosLSF, dd)
	q := findPoly(cosLSF[1:], dd)

	// The symmetric and antisymmetric halves combine into the filter, mirrored about its centre.
	a := make([]int32, order)
	for k := range dd {
		pt := p[k+1] + p[k]
		qt := q[k+1] - q[k]
		a[k] = -qt - pt
		a[order-k-1] = qt - pt
	}

	// The coefficients have to fit in sixteen bits. Where they do not, the filter is widened by an
	// amount judged from how far the largest one overshoots.
	out := make([]int16, order)
	clipped := true
	for range 10 {
		maxAbs, idx := int32(0), 0
		for k := range order {
			if v := abs32(a[k]); v > maxAbs {
				maxAbs, idx = v, k
			}
		}
		maxAbs = rshiftRound(maxAbs, nlsf2aQA+1-12)
		if maxAbs <= 32767 {
			clipped = false
			break
		}
		maxAbs = min(maxAbs, 163838)
		scale := int32(65471) - (maxAbs-32767)<<14/((maxAbs*int32(idx+1))>>2) // 0.999 in Q16
		bwExpand32(a, scale)
	}

	for k := range order {
		out[k] = sat16(rshiftRound(a[k], nlsf2aQA+1-12))
		if clipped {
			// Ten passes were not enough, so the clipped values become the filter and the unscaled
			// ones are brought back into step with them.
			a[k] = int32(out[k]) << (nlsf2aQA + 1 - 12)
		}
	}

	// A filter that fits may still be too resonant to be stable in arithmetic. Widen it until it is,
	// by a little more each time.
	for i := range maxStabiliseIterations {
		if inversePredGain(out) >= minInversePredGainQ30 {
			break
		}
		bwExpand32(a, 65536-int32(2)<<uint(i))
		for k := range order {
			out[k] = int16(rshiftRound(a[k], nlsf2aQA+1-12))
		}
	}
	return out
}
