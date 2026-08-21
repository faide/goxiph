package celt

import "github.com/faide/goxiph/internal/rangecoder"

// interpolate finishes the allocation: it bisects between the two table rows to find the largest
// split the frame can afford, decides which high bands to skip, then divides each band's capacity
// between fine energy and shape.
//
// This is the part RFC 6716 section 4.3.3 requires to be reproduced exactly. Every branch here
// changes how many symbols the shape decoder reads afterwards, so an error does not shift the
// allocation slightly; it desynchronises the rest of the frame.
func (a *Allocation) interpolate(d *rangecoder.Decoder, start, end, skipStart int,
	bits1, bits2, thresh, caps *[NumBands]int,
	total, skipRsv, intensityRsv, dualStereoRsv int, frame FrameSize, channels int,
) {
	allocFloor := channels << BitRes
	stereo := 0
	if channels > 1 {
		stereo = 1
	}
	logM := int(frame) << BitRes

	// Bisect on the interpolation weight rather than solving for it: the sum is monotone in the
	// weight but not smooth, because bands drop out as they fall below their threshold.
	lo, hi := 0, 1<<allocSteps
	for range allocSteps {
		mid := (lo + hi) >> 1
		psum, done := 0, false
		for b := end - 1; b >= start; b-- {
			tmp := bits1[b] + (mid * bits2[b] >> allocSteps)
			if tmp >= thresh[b] || done {
				done = true
				psum += min(tmp, caps[b])
			} else if tmp >= allocFloor {
				psum += allocFloor
			}
		}
		if psum > total {
			hi = mid
		} else {
			lo = mid
		}
	}

	psum, done := 0, false
	for b := end - 1; b >= start; b-- {
		tmp := bits1[b] + (lo * bits2[b] >> allocSteps)
		if tmp < thresh[b] && !done {
			if tmp >= allocFloor {
				tmp = allocFloor
			} else {
				tmp = 0
			}
		} else {
			done = true
		}
		tmp = min(tmp, caps[b])
		a.Pulses[b] = tmp
		psum += tmp
	}

	// Decide which bands to skip, working down from the top.
	codedBands := end
	for ; ; codedBands-- {
		j := codedBands - 1
		// The first band is never skipped, nor is one a boost was spent on: skipping either would
		// mean spending a bit to discard what a bit was just spent to request.
		if j <= skipStart {
			total += skipRsv
			break
		}

		left := total - psum
		width := BandEdges[codedBands] - BandEdges[start]
		perCoeff := left / width
		left -= width * perCoeff
		rem := max(left-(BandEdges[j]-BandEdges[start]), 0)
		bandWidth := BandEdges[codedBands] - BandEdges[j]
		bandBits := a.Pulses[j] + perCoeff*bandWidth + rem

		// A skip decision is only coded when the band could afford one; below that it is skipped
		// unconditionally, which is what guarantees the flag itself is always affordable.
		if bandBits >= max(thresh[j], allocFloor+(1<<BitRes)) {
			if d.DecodeBitLogp(1) != 0 {
				break
			}
			psum += 1 << BitRes
			bandBits -= 1 << BitRes
		}

		// Reclaim what this band held, and reprice the intensity parameter now that it addresses
		// one band fewer.
		psum -= a.Pulses[j] + intensityRsv
		if intensityRsv > 0 {
			intensityRsv = int(log2FracTable[j-start])
		}
		psum += intensityRsv

		if bandBits >= allocFloor {
			psum += allocFloor
			a.Pulses[j] = allocFloor
		} else {
			a.Pulses[j] = 0
		}
	}
	a.CodedBands = codedBands

	if intensityRsv > 0 {
		a.Intensity = start + int(d.DecodeUint(uint32(codedBands+1-start)))
	} else {
		a.Intensity = 0
	}
	if a.Intensity <= start {
		total += dualStereoRsv
		dualStereoRsv = 0
	}
	if dualStereoRsv > 0 {
		a.DualStereo = d.DecodeBitLogp(1) != 0
	}

	// Spread whatever is left evenly across the coded bands, then hand out the remainder a bin at
	// a time so nothing is wasted.
	left := total - psum
	width := BandEdges[codedBands] - BandEdges[start]
	perCoeff := left / width
	left -= width * perCoeff
	for b := start; b < codedBands; b++ {
		a.Pulses[b] += perCoeff * (BandEdges[b+1] - BandEdges[b])
	}
	for b := start; b < codedBands; b++ {
		take := min(left, BandEdges[b+1]-BandEdges[b])
		a.Pulses[b] += take
		left -= take
	}

	a.splitFineAndShape(start, codedBands, end, caps, logM, frame, channels, stereo)
}

// splitFineAndShape divides each band's capacity between fine energy and shape.
//
// Fine energy buys a flat improvement per bit while shape buys less as it grows, so the split
// follows a curve rather than a ratio: the offset below is that curve, expressed as a correction to
// each band's fair share.
func (a *Allocation) splitFineAndShape(start, codedBands, end int, caps *[NumBands]int,
	logM int, frame FrameSize, channels, stereo int,
) {
	balance := 0
	for b := start; b < codedBands; b++ {
		n0 := BandEdges[b+1] - BandEdges[b]
		n := n0 << frame
		a.Pulses[b] += balance

		var excess int
		if n > 1 {
			excess = max(a.Pulses[b]-caps[b], 0)
			a.Pulses[b] -= excess

			// Joint stereo carries one extra degree of freedom, which is paid for here.
			den := channels * n
			if channels == 2 && n > 2 && !a.DualStereo && b < a.Intensity {
				den++
			}
			nclogN := den * (logN[b] + logM)
			offset := (nclogN >> 1) - den*fineOffset

			// Two-bin bands are the one point the curve does not fit.
			if n == 2 {
				offset += den << BitRes >> 2
			}
			// The second and third fine bits are worth more than the curve suggests.
			if a.Pulses[b]+offset < den*2<<BitRes {
				offset += nclogN >> 2
			} else if a.Pulses[b]+offset < den*3<<BitRes {
				offset += nclogN >> 3
			}

			fine := max(0, (a.Pulses[b]+offset+(den<<(BitRes-1)))/(den<<BitRes))
			if channels*fine > a.Pulses[b]>>BitRes {
				fine = a.Pulses[b] >> stereo >> BitRes
			}
			fine = min(fine, MaxFineBits)
			a.FineBits[b] = fine

			// A band rounded down or capped here is a candidate for the final pass.
			a.FinePriority[b] = boolToInt(fine*(den<<BitRes) >= a.Pulses[b]+offset)
			a.Pulses[b] -= channels * fine << BitRes
		} else {
			// A one-bin band spends everything on fine energy but a single sign bit.
			excess = max(0, a.Pulses[b]-(channels<<BitRes))
			a.Pulses[b] -= excess
			a.FineBits[b] = 0
			a.FinePriority[b] = 1
		}

		// Fine energy cannot take part in the shape decoder's rebalancing, so anything over the cap
		// is turned into fine bits here instead.
		if excess > 0 {
			extraFine := min(excess>>(stereo+BitRes), MaxFineBits-a.FineBits[b])
			a.FineBits[b] += extraFine
			extraBits := extraFine * channels << BitRes
			a.FinePriority[b] = boolToInt(extraBits >= excess-balance)
			excess -= extraBits
		}
		balance = excess
	}
	a.Balance = balance

	// A skipped band spends everything it has on fine energy.
	for b := codedBands; b < end; b++ {
		a.FineBits[b] = a.Pulses[b] >> stereo >> BitRes
		a.Pulses[b] = 0
		a.FinePriority[b] = boolToInt(a.FineBits[b] < 1)
	}
}
