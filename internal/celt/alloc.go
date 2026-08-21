package celt

import "github.com/faide/goxiph/internal/rangecoder"

// Bit allocation follows RFC 6716 section 4.3.3.
//
// The allocation is mostly implicit: both sides know the frame's size, so they interpolate a split
// from a fixed table rather than signalling one. That keeps the overhead near zero, which matters
// because every frame must be decodable alone and so could not amortise the signalling anyway.
//
// The specification is blunt about the consequence: the allocation drives the rest of the decode, so
// it must be recovered exactly. A near-miss does not degrade the audio, it corrupts it.

// BitRes is the fractional precision the allocator works in: bits are counted in eighths.
const BitRes = 3

// allocSteps is the number of bisection steps used to interpolate between two table rows.
const allocSteps = 6

// fineOffset biases the split between fine energy and shape, in eighths of a bit.
const fineOffset = 21

// log2FracTable gives a conservative log2 in eighths of a bit, used to price the intensity
// parameter against the bands it can address.
var log2FracTable = [24]uint8{
	0,
	8, 13,
	16, 19, 21, 23,
	24, 26, 27, 28, 29, 30, 31, 32,
	32, 33, 34, 34, 35, 36, 36, 37, 37,
}

// bandAllocation is the static allocation table of RFC 6716 table 57, eleven quality
// levels by twenty-one bands, in units of 1/32 bit per sample.
var bandAllocation = [11][NumBands]uint8{
	{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0},
	{90, 80, 75, 69, 63, 56, 49, 40, 34, 29, 20, 18, 10, 0, 0, 0, 0, 0, 0, 0, 0},
	{110, 100, 90, 84, 78, 71, 65, 58, 51, 45, 39, 32, 26, 20, 12, 0, 0, 0, 0, 0, 0},
	{118, 110, 103, 93, 86, 80, 75, 70, 65, 59, 53, 47, 40, 31, 23, 15, 4, 0, 0, 0, 0},
	{126, 119, 112, 104, 95, 89, 83, 78, 72, 66, 60, 54, 47, 39, 32, 25, 17, 12, 1, 0, 0},
	{134, 127, 120, 114, 103, 97, 91, 85, 78, 72, 66, 60, 54, 47, 41, 35, 29, 23, 16, 10, 1},
	{144, 137, 130, 124, 113, 107, 101, 95, 88, 82, 76, 70, 64, 57, 51, 45, 39, 33, 26, 15, 1},
	{152, 145, 138, 132, 123, 117, 111, 105, 98, 92, 86, 80, 74, 67, 61, 55, 49, 43, 36, 20, 1},
	{162, 155, 148, 142, 133, 127, 121, 115, 108, 102, 96, 90, 84, 77, 71, 65, 59, 53, 46, 30, 1},
	{172, 165, 158, 152, 143, 137, 131, 125, 118, 112, 106, 100, 94, 87, 81, 75, 69, 63, 56, 45, 20},
	{200, 200, 200, 200, 200, 200, 200, 200, 198, 193, 188, 183, 178, 173, 168, 163, 158, 153, 148, 129, 104},
}

// cacheCaps holds the per-band maximum allocation, indexed by (2*frameSize + stereo)
// then by band. RFC 6716 section 4.3.3 states these are used as a table rather than
// recomputed; the procedure that generated them lives in the reference's rate.c.
var cacheCaps = [8][NumBands]uint8{
	{224, 224, 224, 224, 224, 224, 224, 224, 160, 160, 160, 160, 185, 185, 185, 178, 178, 168, 134, 61, 37},
	{224, 224, 224, 224, 224, 224, 224, 224, 240, 240, 240, 240, 207, 207, 207, 198, 198, 183, 144, 66, 40},
	{160, 160, 160, 160, 160, 160, 160, 160, 185, 185, 185, 185, 193, 193, 193, 183, 183, 172, 138, 64, 38},
	{240, 240, 240, 240, 240, 240, 240, 240, 207, 207, 207, 207, 204, 204, 204, 193, 193, 180, 143, 66, 40},
	{185, 185, 185, 185, 185, 185, 185, 185, 193, 193, 193, 193, 193, 193, 193, 183, 183, 172, 138, 65, 39},
	{207, 207, 207, 207, 207, 207, 207, 207, 204, 204, 204, 204, 201, 201, 201, 188, 188, 176, 141, 66, 40},
	{193, 193, 193, 193, 193, 193, 193, 193, 193, 193, 193, 193, 194, 194, 194, 184, 184, 173, 139, 65, 39},
	{204, 204, 204, 204, 204, 204, 204, 204, 201, 201, 201, 201, 198, 198, 198, 187, 187, 175, 140, 66, 40},
}

// logN is a per-band log2 of the band width in the shortest frame, scaled by 1<<BITRES.
var logN = [NumBands]int{0, 0, 0, 0, 0, 0, 0, 0, 8, 8, 8, 8, 16, 16, 16, 21, 21, 24, 29, 34, 36}

// Allocation is the outcome of the bit allocation for one frame.
type Allocation struct {
	// Pulses is the shape allocation per band, in eighths of a bit.
	Pulses [NumBands]int
	// FineBits is the fine energy allocation per band, in bits per channel.
	FineBits [NumBands]int
	// FinePriority orders bands for the final pass that spends leftover bits.
	FinePriority [NumBands]int

	// CodedBands is the band past the last one carrying shape; bands beyond it were skipped.
	CodedBands int
	// Intensity is the first band coded as intensity stereo, or the start band when unused.
	Intensity int
	// DualStereo reports that the channels are coded separately rather than jointly.
	DualStereo bool
	// Balance is capacity left over, which the shape decoder redistributes.
	Balance int
}

// Caps returns the per-band maximum allocation in eighths of a bit.
//
// RFC 6716 section 4.3.3 gives the conversion: the stored value plus 64, scaled by the channel count
// and the band width, divided by four. The maximum is approximate because shape coding is variable
// rate, so it reflects an average rather than a bound.
func Caps(frame FrameSize, channels int) [NumBands]int {
	var caps [NumBands]int
	row := &cacheCaps[2*int(frame)+channels-1]
	for b := range NumBands {
		n := BandEdges[b+1] - BandEdges[b]
		caps[b] = (int(row[b]) + 64) * channels * (n << frame) >> 2
	}
	return caps
}

// DecodeBoosts reads the per-band allocation boosts.
//
// A boost is coded as a run of very unlikely bits, so a frame that uses none pays almost nothing:
// about half a bit across all twenty-one bands. Each boost a band receives makes the next one
// cheaper, which is what lets an encoder concentrate capacity without paying for it repeatedly.
func DecodeBoosts(d *rangecoder.Decoder, offsets *[NumBands]int, start, end int,
	frame FrameSize, channels int, caps *[NumBands]int, totalBits int,
) int {
	dynallocLogp := uint32(6)
	totalBoost := 0

	for b := start; b < end; b++ {
		// The width counts every coded bin, so a stereo band is twice as wide as its bin count.
		width := channels * (BandEdges[b+1] - BandEdges[b]) << frame
		// A boost step is six bits, floored at an eighth of a bit per bin and capped at one.
		quanta := min(8*width, max(48, width))

		boost := 0
		loopLogp := dynallocLogp
		// The budget shrinks as boosts are spent, so the loop closes sooner the more it has given
		// away. Testing the original budget instead reads past where the encoder stopped writing.
		for int(loopLogp)<<BitRes+d.TellFrac() < totalBits && boost < caps[b] {
			if d.DecodeBitLogp(loopLogp) == 0 {
				break
			}
			boost += quanta
			totalBoost += quanta
			totalBits -= quanta
			// Every boost after the first in a band costs a single bit.
			loopLogp = 1
		}
		offsets[b] = boost

		if boost > 0 && dynallocLogp > 2 {
			dynallocLogp--
		}
	}
	return totalBoost
}

// trimICDF is the inverse cumulative distribution of the allocation trim, RFC 6716 table 58.
//
// The trim biases the whole allocation towards low or high frequencies. Five means no bias, and the
// distribution is centred there so that mild adjustments cost least.
var trimICDF = []byte{126, 124, 119, 109, 87, 41, 19, 9, 4, 2, 0}

// DecodeTrim reads the allocation trim, defaulting to five when the frame has no room to signal it.
func DecodeTrim(d *rangecoder.Decoder, totalBits, totalBoost int) int {
	if d.TellFrac()+(6<<BitRes) <= totalBits-totalBoost {
		return d.DecodeICDF(trimICDF, 7)
	}
	return 5
}

// ComputeAllocation interpolates the allocation for one frame and decodes the parameters that go
// with it: which bands are skipped, where intensity stereo begins, and whether the channels are
// coded jointly.
//
// total is the frame's remaining capacity in eighths of a bit.
func ComputeAllocation(d *rangecoder.Decoder, start, end int, offsets *[NumBands]int,
	caps *[NumBands]int, trim int, total int, frame FrameSize, channels int, transient bool,
) Allocation {
	var a Allocation
	a.Intensity = start
	total = max(total, 0)

	// Reserve capacity for the flags that follow, before the interpolation spends it.
	skipRsv := 0
	if total >= 1<<BitRes {
		skipRsv = 1 << BitRes
	}
	total -= skipRsv

	intensityRsv, dualStereoRsv := 0, 0
	if channels == 2 {
		intensityRsv = int(log2FracTable[end-start])
		if intensityRsv > total {
			intensityRsv = 0
		} else {
			total -= intensityRsv
			if total >= 1<<BitRes {
				dualStereoRsv = 1 << BitRes
			}
			total -= dualStereoRsv
		}
	}

	var thresh [NumBands]int
	for b := start; b < end; b++ {
		n := BandEdges[b+1] - BandEdges[b]
		// Below this, a band is better served by nothing at all than by a spectrum too sparse to
		// be worth coding.
		thresh[b] = max(channels<<BitRes, (3*(n<<frame)<<BitRes)>>4)
	}
	trimOffset := TrimOffsets(start, end, trim, frame, channels)

	// Find the two table rows the frame's capacity falls between.
	lo, hi := 1, len(bandAllocation)-1
	for lo <= hi {
		mid := (lo + hi) >> 1
		psum, done := 0, false
		for b := end - 1; b >= start; b-- {
			bits := allocationFor(mid, b, frame, channels, &trimOffset, offsets)
			if bits >= thresh[b] || done {
				done = true
				psum += min(bits, caps[b])
			} else if bits >= channels<<BitRes {
				psum += channels << BitRes
			}
		}
		if psum > total {
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}
	hi = lo
	lo--

	// Build the two bounds the interpolation runs between.
	//
	// A band's boost is added to the upper bound always but to the lower one only when the lower
	// row is not the empty one. Adding it to both would let the lower bound claim capacity the
	// frame does not have, which drives the final share negative.
	var bits1, bits2 [NumBands]int
	skipStart := start
	for b := start; b < end; b++ {
		n := BandEdges[b+1] - BandEdges[b]

		lower := channels * n * int(bandAllocation[lo][b]) << frame >> 2
		upper := caps[b]
		if hi < len(bandAllocation) {
			upper = channels * n * int(bandAllocation[hi][b]) << frame >> 2
		}
		if lower > 0 {
			lower = max(0, lower+trimOffset[b])
		}
		if upper > 0 {
			upper = max(0, upper+trimOffset[b])
		}
		if lo > 0 {
			lower += offsets[b]
		}
		upper += offsets[b]

		// A boosted band is never skipped: skipping it would discard what the boost just asked for.
		if offsets[b] > 0 {
			skipStart = b
		}

		bits1[b] = lower
		bits2[b] = max(0, upper-lower)
	}

	a.interpolate(d, start, end, skipStart, &bits1, &bits2, &thresh, caps,
		total, skipRsv, intensityRsv, dualStereoRsv, frame, channels)
	_ = transient
	return a
}

// allocationFor is one table row's contribution for a band, with the trim and any boost applied.
//
// Used only by the row search, which weighs whole rows and so always counts the boost.
func allocationFor(row, band int, frame FrameSize, channels int,
	trimOffset, offsets *[NumBands]int,
) int {
	n := BandEdges[band+1] - BandEdges[band]
	bits := channels * n * int(bandAllocation[row][band]) << frame >> 2
	if bits > 0 {
		bits = max(0, bits+trimOffset[band])
	}
	return bits + offsets[band]
}

// TrimOffsets returns the per-band bias the allocation trim applies, in eighths of a bit.
//
// The trim tilts the whole allocation across frequency: five is neutral, lower values move capacity
// down and higher values move it up. The tilt is proportional to how many bands remain above the
// one being weighted, which is what makes it a rotation about the top of the spectrum rather than a
// uniform shift.
func TrimOffsets(start, end, trim int, frame FrameSize, channels int) [NumBands]int {
	var offsets [NumBands]int
	for b := start; b < end; b++ {
		n := BandEdges[b+1] - BandEdges[b]
		offsets[b] = channels * n * (trim - 5 - int(frame)) * (end - b - 1) *
			(1 << (uint(frame) + BitRes)) >> 6
		// A single-coefficient band gains more from the coarse energy than from shape, so it is
		// given less. The test is on the bin count, not on the coded width: it does not count
		// channels, though the offset it subtracts does.
		if n<<frame == 1 {
			offsets[b] -= channels << BitRes
		}
	}
	return offsets
}
