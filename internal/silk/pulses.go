package silk

import "github.com/faide/goxiph/internal/rangecoder"

// The excitation is the residual left after both prediction filters, coded as integer pulses. It is
// carried in blocks of sixteen samples, and each block is coded by amplitude rather than sample by
// sample: first how many pulses the block holds in total, then how that total splits between its
// halves, and so on down to individual samples. A block with few pulses therefore costs little
// however they are placed, which is what suits a sparse residual.
//
// Adapted from silk/decode_pulses.c, silk/shell_coder.c and silk/code_signs.c.

// A shell block is sixteen samples, and its pulse count saturates at sixteen before the extra
// magnitude moves into least-significant bits coded separately.
const (
	shellBlockLength = 16
	maxPulses        = 16
	rateLevels       = 10
	// maxLSBShifts bounds the extension: past this the distribution changes so it cannot recur.
	maxLSBShifts = 10
)

// DecodePulses reads a frame's excitation.
func DecodePulses(d *rangecoder.Decoder, signalType, quantOffsetType, frameLength int) []int {
	// The rate level selects how the pulse counts are distributed. Voiced and unvoiced frames have
	// different shapes, so the choice is itself coded from a distribution that depends on which.
	level := d.DecodeICDF(rateLevelICDF[signalType>>1][:], 8)

	blocks := frameLength >> 4
	if blocks*shellBlockLength < frameLength {
		// Ten milliseconds at twelve kilohertz gives 120 samples, which is not a whole number of
		// blocks; the last one is coded in full and its tail discarded.
		blocks++
	}

	// Room for every block, not merely for the frame. The last block of a 120-sample frame runs
	// eight samples past its end, and those samples are still coded and still have to go somewhere.
	pulses := make([]int, blocks*shellBlockLength)

	sums := make([]int, blocks)
	shifts := make([]int, blocks)
	for i := range blocks {
		sums[i] = d.DecodeICDF(pulseCountICDF[level][:], 8)

		// A count at the ceiling means the block holds more than the shell code can carry, and the
		// excess is coded as extra bits per sample. Each repetition doubles the magnitude.
		for sums[i] == maxPulses+1 {
			shifts[i]++
			// The last distribution is offset by one entry once ten shifts have been taken, which
			// removes the escape and so bounds the loop.
			table := pulseCountICDF[rateLevels-1][:]
			if shifts[i] == maxLSBShifts {
				table = table[1:]
			}
			sums[i] = d.DecodeICDF(table, 8)
		}
	}

	for i := range blocks {
		if sums[i] > 0 {
			decodeShellBlock(d, pulses[i*shellBlockLength:], sums[i])
		}
	}

	for i := range blocks {
		if shifts[i] == 0 {
			continue
		}
		block := pulses[i*shellBlockLength:]
		for k := range shellBlockLength {
			v := block[k]
			for range shifts[i] {
				v = v<<1 + d.DecodeICDF(lsbICDF[:], 8)
			}
			block[k] = v
		}
		// A block whose shell held nothing may still carry pulses from the extension, and the sign
		// pass has to know that. Recording the shift count keeps the total non-zero.
		sums[i] |= shifts[i] << 5
	}

	decodeSigns(d, pulses, frameLength, signalType, quantOffsetType, sums)
	return pulses[:frameLength]
}

// decodeShellBlock splits a block's pulse total down to individual samples.
//
// Only one half of each split is coded; the other is the remainder. The tables are indexed by the
// total being split, so a split of few pulses costs less than a split of many.
func decodeShellBlock(d *rangecoder.Decoder, out []int, total int) {
	var half [2]int
	var quarter [4]int
	var eighth [8]int

	split := func(dst *[2]int, from int, table []uint8) {
		if from <= 0 {
			dst[0], dst[1] = 0, 0
			return
		}
		dst[0] = d.DecodeICDF(table[shellCodeOffsets[from]:], 8)
		dst[1] = from - dst[0]
	}
	splitInto := func(a, b *int, from int, table []uint8) {
		var pair [2]int
		split(&pair, from, table)
		*a, *b = pair[0], pair[1]
	}

	splitInto(&half[0], &half[1], total, shellCodeTable3[:])

	splitInto(&quarter[0], &quarter[1], half[0], shellCodeTable2[:])
	splitInto(&eighth[0], &eighth[1], quarter[0], shellCodeTable1[:])
	splitInto(&out[0], &out[1], eighth[0], shellCodeTable0[:])
	splitInto(&out[2], &out[3], eighth[1], shellCodeTable0[:])
	splitInto(&eighth[2], &eighth[3], quarter[1], shellCodeTable1[:])
	splitInto(&out[4], &out[5], eighth[2], shellCodeTable0[:])
	splitInto(&out[6], &out[7], eighth[3], shellCodeTable0[:])

	splitInto(&quarter[2], &quarter[3], half[1], shellCodeTable2[:])
	splitInto(&eighth[4], &eighth[5], quarter[2], shellCodeTable1[:])
	splitInto(&out[8], &out[9], eighth[4], shellCodeTable0[:])
	splitInto(&out[10], &out[11], eighth[5], shellCodeTable0[:])
	splitInto(&eighth[6], &eighth[7], quarter[3], shellCodeTable1[:])
	splitInto(&out[12], &out[13], eighth[6], shellCodeTable0[:])
	splitInto(&out[14], &out[15], eighth[7], shellCodeTable0[:])
}

// decodeSigns attaches a sign to every non-zero pulse.
//
// The magnitudes are coded without sign because a residual is close to symmetric, so a sign is
// nearly one bit whatever else is known. Nearly, not quite: a densely populated block leans very
// slightly one way, which is why the distribution depends on the block's pulse count.
func decodeSigns(d *rangecoder.Decoder, pulses []int, frameLength, signalType, quantOffsetType int, sums []int) {
	base := 7 * (quantOffsetType + signalType<<1)
	blocks := (frameLength + shellBlockLength/2) >> 4

	icdf := [2]byte{0, 0}
	for i := range blocks {
		p := sums[i]
		if p <= 0 {
			continue
		}
		icdf[0] = signICDF[base+min(p&0x1F, 6)]

		block := pulses[i*shellBlockLength:]
		for j := range shellBlockLength {
			if block[j] > 0 {
				// The symbol is zero or one, and maps onto minus one or plus one.
				block[j] *= 2*d.DecodeICDF(icdf[:], 8) - 1
			}
		}
	}
}
