package flac

import (
	"fmt"

	"github.com/faide/goxiph/internal/bitio"
)

// Subframe type codes. RFC 9639 section 9.2.1.
const (
	subframeConstant = 0
	subframeVerbatim = 1
)

// maxLPCOrder is the widest linear predictor the format allows.
const maxLPCOrder = 32

// fixedCoefficients are the five fixed predictors of RFC 9639 section 9.2.5, written as the
// coefficients they apply to the preceding samples.
var fixedCoefficients = [5][]int64{
	{},
	{1},
	{2, -1},
	{3, -3, 1},
	{4, -6, 4, -1},
}

// readSubframe decodes one subframe into out, which must hold blockSize samples.
//
// depth is the subframe's own bit depth: the frame's depth, plus one for a side channel, minus any
// wasted bits. RFC 9639 section 9.2.
func readSubframe(r *bitio.MSBReader, out []int32, depth uint) error {
	pad, err := r.Read(1)
	if err != nil {
		return fmt.Errorf("%w: truncated at subframe header", ErrBadFrame)
	}
	if pad != 0 {
		return fmt.Errorf("%w: subframe header pad bit set", ErrBadFrame)
	}

	kind, err := r.Read(6)
	if err != nil {
		return fmt.Errorf("%w: truncated at subframe type", ErrBadFrame)
	}
	hasWasted, err := r.ReadBool()
	if err != nil {
		return fmt.Errorf("%w: truncated at wasted bits flag", ErrBadFrame)
	}

	var wasted uint
	if hasWasted {
		// The count minus one is unary, so k=3 appears as 0b001.
		n, err := r.ReadUnary(int(depth))
		if err != nil {
			return fmt.Errorf("%w: reading wasted bits: %w", ErrBadFrame, err)
		}
		wasted = uint(n) + 1
		if wasted >= depth {
			return fmt.Errorf("%w: %d wasted bits leaves no depth from %d", ErrBadFrame, wasted, depth)
		}
		depth -= wasted
	}

	switch {
	case kind == subframeConstant:
		err = readConstant(r, out, depth)
	case kind == subframeVerbatim:
		err = readVerbatim(r, out, depth)
	case kind >= 8 && kind <= 12:
		err = readFixed(r, out, depth, int(kind)-8)
	case kind >= 32:
		err = readLPC(r, out, depth, int(kind)-31)
	default:
		return fmt.Errorf("%w: subframe type %#02x is reserved", ErrBadFrame, kind)
	}
	if err != nil {
		return err
	}

	// Wasted bits are restored by shifting left, before any stereo decorrelation is undone.
	if wasted > 0 {
		for i := range out {
			out[i] <<= wasted
		}
	}
	return nil
}

func readConstant(r *bitio.MSBReader, out []int32, depth uint) error {
	v, err := r.ReadSigned(depth)
	if err != nil {
		return fmt.Errorf("%w: truncated at constant sample", ErrBadFrame)
	}
	for i := range out {
		out[i] = int32(v)
	}
	return nil
}

func readVerbatim(r *bitio.MSBReader, out []int32, depth uint) error {
	for i := range out {
		v, err := r.ReadSigned(depth)
		if err != nil {
			return fmt.Errorf("%w: truncated at verbatim sample %d", ErrBadFrame, i)
		}
		out[i] = int32(v)
	}
	return nil
}

// readFixed decodes a fixed-predictor subframe. RFC 9639 section 9.2.5.
func readFixed(r *bitio.MSBReader, out []int32, depth uint, order int) error {
	if order > len(out) {
		return fmt.Errorf("%w: fixed order %d exceeds block size %d", ErrBadFrame, order, len(out))
	}
	for i := range order {
		v, err := r.ReadSigned(depth)
		if err != nil {
			return fmt.Errorf("%w: truncated at warm-up sample %d", ErrBadFrame, i)
		}
		out[i] = int32(v)
	}
	if err := readResidual(r, out, order, order); err != nil {
		return err
	}

	coeffs := fixedCoefficients[order]
	for i := order; i < len(out); i++ {
		var pred int64
		for j, c := range coeffs {
			pred += c * int64(out[i-1-j])
		}
		out[i] += int32(pred)
	}
	return nil
}

// readLPC decodes a linear-predictor subframe. RFC 9639 section 9.2.6.
func readLPC(r *bitio.MSBReader, out []int32, depth uint, order int) error {
	if order > maxLPCOrder {
		return fmt.Errorf("%w: LPC order %d exceeds %d", ErrBadFrame, order, maxLPCOrder)
	}
	if order > len(out) {
		return fmt.Errorf("%w: LPC order %d exceeds block size %d", ErrBadFrame, order, len(out))
	}

	for i := range order {
		v, err := r.ReadSigned(depth)
		if err != nil {
			return fmt.Errorf("%w: truncated at warm-up sample %d", ErrBadFrame, i)
		}
		out[i] = int32(v)
	}

	precBits, err := r.Read(4)
	if err != nil {
		return fmt.Errorf("%w: truncated at coefficient precision", ErrBadFrame)
	}
	if precBits == 0b1111 {
		return fmt.Errorf("%w: coefficient precision code 15 is forbidden", ErrBadFrame)
	}
	precision := uint(precBits) + 1

	shift, err := r.ReadSigned(5)
	if err != nil {
		return fmt.Errorf("%w: truncated at prediction shift", ErrBadFrame)
	}
	if shift < 0 {
		return fmt.Errorf("%w: prediction shift %d is negative", ErrBadFrame, shift)
	}

	var coeffs [maxLPCOrder]int64
	for i := range order {
		c, err := r.ReadSigned(precision)
		if err != nil {
			return fmt.Errorf("%w: truncated at coefficient %d", ErrBadFrame, i)
		}
		coeffs[i] = c
	}

	if err := readResidual(r, out, order, order); err != nil {
		return err
	}

	// Coefficients run backwards in time: the first applies to the sample directly before the one
	// being predicted. int64 accumulation is required, not an optimisation: a 32-bit depth with a
	// 15-bit coefficient over 32 taps overflows 32 bits comfortably.
	for i := order; i < len(out); i++ {
		var pred int64
		for j := range order {
			pred += coeffs[j] * int64(out[i-1-j])
		}
		out[i] += int32(pred >> uint(shift))
	}
	return nil
}

// readResidual decodes the partitioned Rice-coded residual into out[start:].
// RFC 9639 section 9.2.7.
func readResidual(r *bitio.MSBReader, out []int32, start, order int) error {
	method, err := r.Read(2)
	if err != nil {
		return fmt.Errorf("%w: truncated at residual coding method", ErrBadFrame)
	}
	if method > 1 {
		return fmt.Errorf("%w: residual coding method %d is reserved", ErrBadFrame, method)
	}
	paramBits := uint(4)
	escape := uint64(0b1111)
	if method == 1 {
		paramBits = 5
		escape = 0b11111
	}

	orderBits, err := r.Read(4)
	if err != nil {
		return fmt.Errorf("%w: truncated at partition order", ErrBadFrame)
	}
	partitions := 1 << orderBits

	blockSize := len(out)
	if blockSize%partitions != 0 {
		return fmt.Errorf("%w: block size %d is not divisible by %d partitions", ErrBadFrame, blockSize, partitions)
	}
	perPartition := blockSize / partitions
	if perPartition <= order {
		return fmt.Errorf("%w: %d samples per partition does not exceed order %d", ErrBadFrame, perPartition, order)
	}

	pos := start
	for p := range partitions {
		count := perPartition
		if p == 0 {
			count -= order
		}
		if pos+count > blockSize {
			return fmt.Errorf("%w: partition %d overruns the block", ErrBadFrame, p)
		}

		param, err := r.Read(paramBits)
		if err != nil {
			return fmt.Errorf("%w: truncated at partition %d parameter", ErrBadFrame, p)
		}

		if param == escape {
			// An escaped partition stores its residuals unencoded at a signalled width, which may
			// be zero: every residual is then zero and no bits are stored at all.
			width, err := r.Read(5)
			if err != nil {
				return fmt.Errorf("%w: truncated at partition %d escape width", ErrBadFrame, p)
			}
			for i := range count {
				if width == 0 {
					out[pos+i] = 0
					continue
				}
				v, err := r.ReadSigned(uint(width))
				if err != nil {
					return fmt.Errorf("%w: truncated at escaped residual %d", ErrBadFrame, i)
				}
				out[pos+i] = int32(v)
			}
			pos += count
			continue
		}

		for i := range count {
			v, err := readRice(r, uint(param))
			if err != nil {
				return fmt.Errorf("%w: partition %d sample %d: %w", ErrBadFrame, p, i, err)
			}
			out[pos+i] = v
		}
		pos += count
	}
	return nil
}

// readRice decodes one Rice code word. RFC 9639 section 9.2.7.2.
//
// The value is zigzag folded: an even folded value halves, an odd one halves and inverts. That maps
// the unsigned code back onto a signed residual without a sign bit.
func readRice(r *bitio.MSBReader, param uint) (int32, error) {
	// The quotient bounds the run of zeros; anything longer is corrupt rather than large.
	quotient, err := r.ReadUnary(1 << 20)
	if err != nil {
		return 0, err
	}
	remainder, err := r.Read(param)
	if err != nil {
		return 0, err
	}
	folded := uint32(quotient)<<param | uint32(remainder)
	if folded&1 == 0 {
		return int32(folded >> 1), nil
	}
	return int32(^(folded >> 1)), nil
}
