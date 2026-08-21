package silk

import "math/bits"

// SILK is specified in fixed point, and its arithmetic is part of the format rather than an
// implementation choice: the decoder's output feeds back into its own predictor state, so a
// rounding that differs anywhere diverges from there onward. These mirror the reference's macros
// exactly, including where they truncate rather than round.
//
// Adapted from silk/macros.h, silk/SigProc_FIX.h and silk/Inlines.h.

// smulwb multiplies a 32-bit value by the low half of another, keeping the top 32 bits.
//
// It is written as two halves rather than as a 64-bit product because that is what the reference
// does, and the two disagree on how the low half rounds.
func smulwb(a, b int32) int32 {
	return (a>>16)*int32(int16(b)) + ((a&0xFFFF)*int32(int16(b)))>>16
}

// smlawb is smulwb accumulated onto a running total.
func smlawb(a, b, c int32) int32 {
	return a + (b>>16)*int32(int16(c)) + ((b&0xFFFF)*int32(int16(c)))>>16
}

// smulbb multiplies the low halves of two values.
func smulbb(a, b int32) int32 {
	return int32(int16(a)) * int32(int16(b))
}

// rshiftRound shifts right with rounding away from zero at the halfway point.
func rshiftRound(a int32, shift uint) int32 {
	if shift == 1 {
		return a>>1 + a&1
	}
	return ((a >> (shift - 1)) + 1) >> 1
}

// ror32 rotates right.
func ror32(a int32, rot uint) int32 {
	return int32(bits.RotateLeft32(uint32(a), -int(rot)))
}

// sqrtApprox returns an approximate square root in Q7 relative to its input's scale.
//
// The approximation is part of the format: it feeds the line-spectral weights, and a more accurate
// root would give different coefficients from every other decoder.
func sqrtApprox(x int32) int32 {
	if x <= 0 {
		return 0
	}

	lz := int32(bits.LeadingZeros32(uint32(x)))
	fracQ7 := ror32(x, uint(24-lz)) & 0x7F

	// A square root halves the exponent, so an odd number of leading zeros lands between powers of
	// two and starts from the root of two instead.
	y := int32(46214) // sqrt(2) in Q15
	if lz&1 != 0 {
		y = 32768
	}
	y >>= uint(lz >> 1)

	// One Newton step, expressed as a correction proportional to the fractional part.
	return smlawb(y, y, smulbb(213, fracQ7))
}

// log2lin converts a base-two logarithm in Q7 to a linear value in Q16.
//
// The curve between powers of two is a parabola rather than the true exponential, which is close
// enough over one octave and cheap.
func log2lin(inLogQ7 int32) int32 {
	if inLogQ7 < 0 {
		return 0
	}

	out := int32(1) << uint(inLogQ7>>7)
	fracQ7 := inLogQ7 & 0x7F
	correction := smlawb(fracQ7, smulbb(fracQ7, 128-fracQ7), -174)

	if inLogQ7 < 2048 {
		return out + (out*correction)>>7
	}
	return out + (out>>7)*correction
}

// limit clamps v to the inclusive range.
func limit(v, lo, hi int32) int32 {
	return min(max(v, lo), hi)
}

// smulww multiplies two 32-bit values keeping the top 32 bits, in the reference's split form.
func smulww(a, b int32) int32 {
	return smulwb(a, b) + a*rshiftRound(b, 16)
}

// smlaww accumulates smulww onto a running total.
func smlaww(a, b, c int32) int32 {
	return smlawb(a, b, c) + b*rshiftRound(c, 16)
}

// smmul returns the top 32 bits of a 64-bit product.
func smmul(a, b int32) int32 {
	return int32((int64(a) * int64(b)) >> 32)
}

// mulFracQ multiplies and shifts back with rounding, in 64 bits so the product does not wrap.
func mulFracQ(a, b int32, q uint) int32 {
	return int32(rshiftRound64(int64(a)*int64(b), q))
}

func rshiftRound64(a int64, shift uint) int64 {
	if shift == 1 {
		return a>>1 + a&1
	}
	return ((a >> (shift - 1)) + 1) >> 1
}

// sat16 clamps to the range of a signed 16-bit value.
func sat16(a int32) int16 {
	return int16(limit(a, -32768, 32767))
}

// inverse32VarQ approximates one shifted left by qres, divided by b.
//
// A plain division would be exact, but the reference's two-step approximation is what the format
// specifies, and the two differ in the last bits of the filter stability check that uses it.
func inverse32VarQ(b int32, qres uint) int32 {
	if b == 0 {
		return 0
	}

	headroom := uint(bits.LeadingZeros32(uint32(abs32(b)))) - 1
	normalised := b << headroom

	// A first inverse good to fourteen bits, then one refinement from the residual.
	invB := (int32(0x7FFFFFFF) >> 2) / (normalised >> 16)
	result := invB << 16
	errQ32 := ((1 << 29) - smulwb(normalised, invB)) << 3
	result = smlaww(result, errQ32, invB)

	shift := 61 - int(headroom) - int(qres)
	if shift <= 0 {
		return lshiftSat32(result, uint(-shift))
	}
	if shift < 32 {
		return result >> uint(shift)
	}
	return 0
}

func abs32(a int32) int32 {
	if a < 0 {
		return -a
	}
	return a
}

// lshiftSat32 shifts left, clamping rather than wrapping.
func lshiftSat32(a int32, shift uint) int32 {
	return limit(a, int32(-2147483648)>>shift, int32(2147483647)>>shift) << shift
}

// div32VarQ approximates a divided by b, shifted left by qres.
//
// Like inverse32VarQ this is an approximation the format specifies rather than a plain division:
// its result adjusts the filter state between subframes, so a decoder that divided exactly would
// carry a different state forward.
func div32VarQ(a, b int32, qres uint) int32 {
	if b == 0 {
		return 0
	}

	aHead := uint(bits.LeadingZeros32(uint32(abs32(a)))) - 1
	bHead := uint(bits.LeadingZeros32(uint32(abs32(b)))) - 1
	aNorm := a << aHead
	bNorm := b << bHead

	invB := (int32(0x7FFFFFFF) >> 2) / (bNorm >> 16)
	result := smulwb(aNorm, invB)
	// The residual is allowed to wrap; what is left of it after the subtraction is always small.
	aNorm -= smmul(bNorm, result) << 3
	result = smlawb(result, aNorm, invB)

	shift := 29 + int(aHead) - int(bHead) - int(qres)
	if shift < 0 {
		return lshiftSat32(result, uint(-shift))
	}
	if shift < 32 {
		return result >> uint(shift)
	}
	return 0
}

// randStep advances the sign randomiser the excitation uses. It wraps, which is the point.
func randStep(seed int32) int32 {
	return 907633515 + seed*196314165
}
