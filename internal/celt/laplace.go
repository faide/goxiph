package celt

import "github.com/faide/goxiph/internal/rangecoder"

// The Laplace coder models a value whose probability falls geometrically with its magnitude, which
// is how coarse energy prediction errors are distributed.
//
// Adapted from celt/laplace.c of the reference implementation in RFC 6716, BSD-3-Clause. The
// specification names the function and its role but does not describe the procedure, so the
// reference is the only normative statement of it. See NOTICE.
const (
	// laplaceLogMinP is the log2 of the floor probability every value keeps, so that no value is
	// impossible to code however far it sits from zero.
	laplaceLogMinP = 0
	laplaceMinP    = 1 << laplaceLogMinP

	// laplaceNMin bounds the range over which the floor probability is reserved.
	laplaceNMin = 16
)

// laplaceFreq1 returns the frequency of the first decaying step, given the frequency of zero and the
// decay rate.
func laplaceFreq1(fs0 uint32, decay int32) uint32 {
	ft := uint32(32768 - laplaceMinP*(2*laplaceNMin))
	if fs0 <= ft {
		ft -= fs0
	} else {
		ft = 0
	}
	return ft * uint32(16384-decay) >> 15
}

// LaplaceDecode reads one Laplace-distributed value.
//
// fs is the frequency assigned to zero and decay controls how fast the tails fall; both come from a
// model table indexed by frame size and band.
func LaplaceDecode(d *rangecoder.Decoder, fs uint32, decay int32) int {
	fm := d.DecodeBin(15)

	val := 0
	fl := uint32(0)
	if fm >= fs {
		val++
		fl = fs
		fs = laplaceFreq1(fs, decay) + laplaceMinP

		// Walk down the decaying part of the distribution.
		for fs > laplaceMinP && fm >= fl+2*fs {
			fs *= 2
			fl += fs
			fs = ((fs - 2*laplaceMinP) * uint32(decay)) >> 15
			fs += laplaceMinP
			val++
		}

		// Past that point every value shares the floor probability, so the remaining distance is a
		// plain division rather than another walk.
		if fs <= laplaceMinP {
			di := int((fm - fl) >> (laplaceLogMinP + 1))
			val += di
			fl += uint32(2 * di * laplaceMinP)
		}

		if fm < fl+fs {
			val = -val
		} else {
			fl += fs
		}
	}

	d.Update(fl, min(fl+fs, 32768), 32768)
	return val
}

// LaplaceEncode writes one Laplace-distributed value, returning the value it coded.
//
// A value beyond what the model can express is clamped, and the clamped value is returned so the
// caller can keep its own state in step with a decoder's.
func LaplaceEncode(e *rangecoder.Encoder, value int, fs uint32, decay int32) int {
	fl := uint32(0)
	val := value

	if val != 0 {
		// Fold the sign out, remembering it as a mask.
		s := 0
		if val < 0 {
			s = -1
		}
		val = (val + s) ^ s

		fl = fs
		fs = laplaceFreq1(fs, decay)

		i := 1
		for ; fs > 0 && i < val; i++ {
			fs *= 2
			fl += fs + 2*laplaceMinP
			fs = (fs * uint32(decay)) >> 15
		}

		if fs == 0 {
			ndiMax := int((32768 - fl + laplaceMinP - 1) >> laplaceLogMinP)
			ndiMax = (ndiMax - s) >> 1
			di := min(val-i, ndiMax-1)
			fl += uint32((2*di + 1 + s) * laplaceMinP)
			fs = min(laplaceMinP, 32768-fl)
			value = (i + di + s) ^ s
		} else {
			fs += laplaceMinP
			fl += fs &^ uint32(s)
		}
	}

	e.EncodeBin(fl, min(fl+fs, 32768), 15)
	return value
}
