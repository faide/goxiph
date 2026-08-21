package celt

import (
	"math"
	"math/bits"
	"sync"

	"github.com/faide/goxiph/internal/rangecoder"
)

// A band is coded as a unit-norm shape, but a wide band at a decent rate needs more pulses than one
// codebook can address. The answer is to split it: code the angle between the two halves, give each
// half a share of the bits, and recurse. The same machinery carries stereo, where the two halves are
// the mid and side channels rather than two halves of a spectrum.
//
// Adapted from celt/bands.c, which RFC 6716 section 4.3.4.4 names without stating the procedure.

// bandDecoder holds the state threaded through the whole recursion.
type bandDecoder struct {
	dec       *rangecoder.Decoder
	cache     *pulseCache
	remaining int    // bits left in the frame, in eighths
	seed      uint32 // advances across every noise fill in the frame
	spread    int
	intensity int
}

// bandArgs is one call's worth of the recursion.
//
// It is a struct because the recursion changes three or four fields at a time and leaves a dozen
// alone; as a parameter list the call sites would be unreadable.
type bandArgs struct {
	band     int
	x, y     []float32 // y is nil for a mono band
	n        int
	b        int // budget in eighths of a bit
	blocks   int
	tfChange int

	lowband    []float32 // the spectrum to fold from, or nil to fold from noise
	lm         int
	lowbandOut []float32 // where to leave this band's spectrum for later bands to fold from
	level      int
	gain       float32
	scratch    []float32
	fill       int // which blocks of the folding source hold anything
}

// isqrt32 returns the integer square root, from celt/mathops.c.
//
// The triangular angle distribution inverts a sum of consecutive integers, so this has to floor
// exactly; a square root taken in floating point could land on the wrong side of a perfect square
// and decode a different angle.
func isqrt32(val uint32) int {
	if val == 0 {
		return 0
	}
	g := uint32(0)
	bshift := (bits.Len32(val) - 1) >> 1
	b := uint32(1) << uint(bshift)
	for bshift >= 0 {
		t := ((g << 1) + b) << uint(bshift)
		if t <= val {
			g += b
			val -= t
		}
		b >>= 1
		bshift--
	}
	return int(g)
}

// decodeTheta reads the split angle.
//
// Three distributions, by what the split means. A stereo band's angle clusters near the middle, so
// it gets a step; a mono band split in frequency is likelier to be balanced, so it gets a triangle;
// a split across time blocks has no preferred angle and gets a uniform.
func (bd *bandDecoder) decodeTheta(qn, n, b0 int, stereo bool) int {
	switch {
	case stereo && n > 2:
		const p0 = 3
		x0 := qn / 2
		ft := p0*(x0+1) + x0

		fs := int(bd.dec.Decode(uint32(ft)))
		var x int
		if fs < (x0+1)*p0 {
			x = fs / p0
		} else {
			x = x0 + 1 + (fs - (x0+1)*p0)
		}

		fl, fh := p0*x, p0*(x+1)
		if x > x0 {
			fl, fh = (x-1-x0)+(x0+1)*p0, (x-x0)+(x0+1)*p0
		}
		bd.dec.Update(uint32(fl), uint32(fh), uint32(ft))
		return x

	case b0 > 1 || stereo:
		return int(bd.dec.DecodeUint(uint32(qn + 1)))

	default:
		half := qn >> 1
		ft := (half + 1) * (half + 1)
		fm := int(bd.dec.Decode(uint32(ft)))

		var itheta, fs, fl int
		if fm < half*(half+1)>>1 {
			itheta = (isqrt32(uint32(8*fm+1)) - 1) >> 1
			fs = itheta + 1
			fl = itheta * (itheta + 1) >> 1
		} else {
			itheta = (2*(qn+1) - isqrt32(uint32(8*(ft-fm-1)+1))) >> 1
			fs = qn + 1 - itheta
			fl = ft - ((qn + 1 - itheta) * (qn + 2 - itheta) >> 1)
		}
		bd.dec.Update(uint32(fl), uint32(fl+fs), uint32(ft))
		return itheta
	}
}

// decodeSign reads the one bit a single-coefficient band carries, when there is room for it.
func (bd *bandDecoder) decodeSign(x []float32) {
	sign := float32(1)
	if bd.remaining >= 1<<BitRes {
		if bd.dec.DecodeBits(1) != 0 {
			sign = -1
		}
		bd.remaining -= 1 << BitRes
	}
	x[0] = sign
}

// stereoMerge turns a decoded mid and side back into left and right.
//
// The two are orthogonal and the side already carries its own scale, so the norms of their sum and
// difference come out of the inner product without reconstructing either channel first.
func stereoMerge(x, y []float32, mid float32) {
	var xp, side float64
	for j := range x {
		xp += float64(x[j]) * float64(y[j])
		side += float64(y[j]) * float64(y[j])
	}
	xp *= float64(mid)

	m2 := float64(mid) * float64(mid)
	el := m2 + side - 2*xp
	er := m2 + side + 2*xp

	// One channel having collapsed leaves nothing to scale by, so both take the mid.
	if er < 6e-4 || el < 6e-4 {
		copy(y, x)
		return
	}

	lgain := 1 / math.Sqrt(el)
	rgain := 1 / math.Sqrt(er)
	for j := range x {
		l := float64(mid) * float64(x[j])
		r := float64(y[j])
		x[j] = float32(lgain * (l - r))
		y[j] = float32(rgain * (l + r))
	}
}

// decodeBand decodes one band into x, and y as well when the band is stereo. It returns the collapse
// mask: which of the band's time blocks received anything.
func (bd *bandDecoder) decodeBand(a bandArgs) uint32 {
	n, b, lm, blocks, fill := a.n, a.b, a.lm, a.blocks, a.fill
	x, y, lowband := a.x, a.y, a.lowband
	tfChange := a.tfChange

	n0 := n
	b0 := blocks
	longBlocks := b0 == 1
	nB := n / blocks
	nB0 := nB

	timeDivide, recombine := 0, 0
	inv := false
	var cm uint32
	var mid, side float32

	stereo := y != nil
	split := stereo

	// A one-coefficient band has no shape to code, only a sign.
	if n == 1 {
		bd.decodeSign(x)
		if stereo {
			bd.decodeSign(y)
		}
		if a.lowbandOut != nil {
			a.lowbandOut[0] = x[0]
		}
		return 1
	}

	if !stereo && a.level == 0 {
		if tfChange > 0 {
			recombine = tfChange
		}

		// The folding source is about to be reshaped in place, and later bands still need it as it
		// stands. Copying it first keeps the reshaping local to this band.
		if lowband != nil && (recombine != 0 || (nB&1) == 0 && tfChange < 0 || b0 > 1) {
			copy(a.scratch[:n], lowband[:n])
			lowband = a.scratch
		}

		// Merge blocks back together to buy frequency resolution.
		for k := range recombine {
			if lowband != nil {
				haar1(lowband, n>>uint(k), 1<<uint(k))
			}
			fill = int(bitInterleaveTable[fill&0xF]) | int(bitInterleaveTable[fill>>4])<<2
		}
		// A valid stream cannot ask to merge more blocks than the frame has: only a transient frame
		// codes a positive change, and a transient frame runs on the full block count. The floor is
		// here so a caller that wired the two together wrongly gets a wrong band rather than a panic.
		recombine = min(recombine, blocksShift(blocks))
		blocks >>= uint(recombine)
		nB <<= uint(recombine)

		// Split blocks apart to buy time resolution.
		for nB&1 == 0 && tfChange < 0 {
			if lowband != nil {
				haar1(lowband, nB, blocks)
			}
			fill |= fill << uint(blocks)
			blocks <<= 1
			nB >>= 1
			timeDivide++
			tfChange++
		}
		b0, nB0 = blocks, nB

		// Lay the blocks out in time order rather than frequency order.
		if b0 > 1 && lowband != nil {
			deinterleaveHadamard(lowband, nB>>uint(recombine), b0<<uint(recombine), longBlocks)
		}
	}

	// Split when the band wants more than about one and a half bits per sample more than its widest
	// codebook can express.
	if !stereo && lm != -1 && n > 2 {
		if run := bd.cache.run(lm, a.band); run != nil && b > int(run[run[0]])+12 {
			n >>= 1
			y = x[n:]
			split = true
			lm--
			if blocks == 1 {
				fill = (fill & 1) | (fill << 1)
			}
			blocks = (blocks + 1) >> 1
		}
	}

	if split {
		var itheta, qalloc, delta int

		pulseCap := logN[a.band] + lm*(1<<BitRes)
		qn := computeQn(n, b, thetaOffset(pulseCap, n, stereo), pulseCap, stereo)
		if stereo && a.band >= bd.intensity {
			qn = 1
		}

		tell := bd.dec.TellFrac()
		switch {
		case qn != 1:
			itheta = bd.decodeTheta(qn, n, b0, stereo) * 16384 / qn
		case stereo:
			// With no angle to code, a stereo band still needs to know whether the side was inverted.
			if b > 2<<BitRes && bd.remaining > 2<<BitRes {
				inv = bd.dec.DecodeBitLogp(2) != 0
			}
		}
		qalloc = bd.dec.TellFrac() - tell
		b -= qalloc

		origFill := fill
		var imid, iside int32
		switch itheta {
		case 0:
			imid, iside = 32767, 0
			fill &= (1 << uint(blocks)) - 1
			delta = -16384
		case 16384:
			imid, iside = 0, 32767
			fill &= ((1 << uint(blocks)) - 1) << uint(blocks)
			delta = 16384
		default:
			imid = bitexactCos(int32(itheta))
			iside = bitexactCos(int32(16384 - itheta))
			// The split that minimises squared error puts the bits where the energy is.
			delta = int(fracMul16(int32((n-1)<<7), bitexactLog2Tan(iside, imid)))
		}
		mid = float32(imid) / 32768
		side = float32(iside) / 32768

		if n == 2 && stereo {
			// Mid and side are orthogonal, so a two-sample side is determined by the mid up to one sign.
			mbits := b
			sbits := 0
			if itheta != 0 && itheta != 16384 {
				sbits = 1 << BitRes
			}
			mbits -= sbits
			bd.remaining -= qalloc + sbits

			x2, y2 := x, y
			if itheta > 8192 {
				x2, y2 = y, x
			}

			sign := float32(1)
			if sbits != 0 && bd.dec.DecodeBits(1) != 0 {
				sign = -1
			}

			sub := a
			sub.x, sub.y, sub.n, sub.b = x2, nil, n, mbits
			sub.blocks, sub.lowband, sub.lm, sub.fill = blocks, lowband, lm, origFill
			cm = bd.decodeBand(sub)

			y2[0] = -sign * x2[1]
			y2[1] = sign * x2[0]

			x[0] *= mid
			x[1] *= mid
			y[0] *= side
			y[1] *= side
			t0, t1 := x[0], x[1]
			x[0], y[0] = t0-y[0], t0+y[0]
			x[1], y[1] = t1-y[1], t1+y[1]
		} else {
			// Give the quieter half of a time split more than its share, to mask pre-echo.
			if b0 > 1 && !stereo && itheta&0x3FFF != 0 {
				if itheta > 8192 {
					delta -= delta >> uint(4-lm)
				} else {
					delta = min(0, delta+(n<<BitRes>>uint(5-lm)))
				}
			}
			mbits := max(0, min(b, (b-delta)/2))
			sbits := b - mbits
			bd.remaining -= qalloc

			var nextLowband2 []float32
			if lowband != nil && !stereo {
				nextLowband2 = lowband[n:]
			}
			var nextLowbandOut1 []float32
			nextLevel := 0
			if stereo {
				nextLowbandOut1 = a.lowbandOut
			} else {
				nextLevel = a.level + 1
			}

			// A stereo mid stays normalised, because later bands fold from it.
			midGain := a.gain * mid
			if stereo {
				midGain = 1
			}
			// A mono split puts the side half in the later time blocks, so its mask belongs further up.
			// The two halves of a stereo split cover the same blocks, so that one does not shift.
			shift := uint(0)
			if !stereo {
				shift = uint(b0 >> 1)
			}

			mSub := a
			mSub.x, mSub.y, mSub.n = x, nil, n
			mSub.blocks, mSub.lowband, mSub.lm = blocks, lowband, lm
			mSub.lowbandOut, mSub.level, mSub.gain, mSub.fill = nextLowbandOut1, nextLevel, midGain, fill

			sSub := a
			sSub.x, sSub.y, sSub.n = y, nil, n
			sSub.blocks, sSub.lowband, sSub.lm = blocks, nextLowband2, lm
			sSub.lowbandOut, sSub.level, sSub.gain = nil, nextLevel, a.gain*side
			sSub.scratch, sSub.fill = nil, fill>>uint(blocks)

			// Whichever half is coded first may leave bits behind; the other one takes them.
			rebalance := bd.remaining
			if mbits >= sbits {
				mSub.b = mbits
				cm = bd.decodeBand(mSub)
				rebalance = mbits - (rebalance - bd.remaining)
				if rebalance > 3<<BitRes && itheta != 0 {
					sbits += rebalance - (3 << BitRes)
				}
				sSub.b = sbits
				cm |= bd.decodeBand(sSub) << shift
			} else {
				sSub.b = sbits
				cm = bd.decodeBand(sSub) << shift
				rebalance = sbits - (rebalance - bd.remaining)
				if rebalance > 3<<BitRes && itheta != 16384 {
					mbits += rebalance - (3 << BitRes)
				}
				mSub.b = mbits
				cm |= bd.decodeBand(mSub)
			}
		}
	} else {
		q, cost := bd.cache.pulsesForBudget(lm, a.band, b, bd.remaining)
		bd.remaining -= cost

		if q != 0 {
			k := getPulses(q)
			pulses := make([]int, n)
			DecodePVQ(bd.dec, pulses, k)

			// The mask has to come off the pulses, before the rotation spreads them.
			cm = CollapseMask(pulses, blocks)
			Normalise(pulses, x[:n])
			for j := range n {
				x[j] *= a.gain
			}
			Rotate(x[:n], blocks, k, bd.spread, false)
		} else {
			// No pulses, so the band is filled rather than left silent: silence in one band of an
			// otherwise busy spectrum is more audible than the wrong noise.
			mask := uint32(1)<<uint(blocks) - 1
			fill &= int(mask)
			switch {
			case fill == 0:
				clear(x[:n])
			case lowband == nil:
				for j := range n {
					bd.seed = lcgRand(bd.seed)
					x[j] = float32(int32(bd.seed) >> 20)
				}
				cm = mask
				renormalise(x[:n], a.gain)
			default:
				// Fold a lower band in, dithered about 48 dB down so it does not read as a copy.
				for j := range n {
					bd.seed = lcgRand(bd.seed)
					tmp := float32(1.0 / 256)
					if bd.seed&0x8000 == 0 {
						tmp = -tmp
					}
					x[j] = lowband[j] + tmp
				}
				cm = uint32(fill)
				renormalise(x[:n], a.gain)
			}
		}
	}

	// Undo everything that was done on the way in.
	switch {
	case stereo:
		if n != 2 {
			stereoMerge(x[:n], y[:n], mid)
		}
		if inv {
			for j := range n {
				y[j] = -y[j]
			}
		}

	case a.level == 0:
		if b0 > 1 {
			interleaveHadamard(x[:n0], nB>>uint(recombine), b0<<uint(recombine), longBlocks)
		}

		nB, blocks = nB0, b0
		for range timeDivide {
			blocks >>= 1
			nB <<= 1
			cm |= cm >> uint(blocks)
			haar1(x, nB, blocks)
		}
		for k := range recombine {
			cm = uint32(bitDeinterleaveTable[cm&0xF])
			haar1(x, n0>>uint(k), 1<<uint(k))
		}
		blocks <<= uint(recombine)

		// Later bands fold from this one, and they expect it scaled by the band width.
		if a.lowbandOut != nil {
			scale := float32(math.Sqrt(float64(n0)))
			for j := range n0 {
				a.lowbandOut[j] = scale * x[j]
			}
		}
		cm &= 1<<uint(blocks) - 1
	}

	return cm
}

// defaultCache is the cost cache for the standard band layout. It is immutable once built, and
// building it costs a few thousand additions, so one copy is shared.
var defaultCache = sync.OnceValue(newPulseCache)

// BandParams carries everything the band loop needs that the frame decoder has already read.
type BandParams struct {
	Start, End    int
	X, Y          []float32 // one slice per channel, TotalBins long; Y nil for mono
	CollapseMasks []byte    // NumBands per channel, written by the loop
	Pulses        []int     // the allocator's per-band budget, in eighths of a bit
	TFRes         []int     // per-band time-frequency change
	ShortBlocks   bool
	Spread        int
	DualStereo    bool
	Intensity     int
	TotalBits     int // frame size in eighths of a bit, less any anti-collapse reservation
	Balance       int
	LM            int
	CodedBands    int
	Seed          uint32
}

// DecodeBands decodes every coded band of a frame and returns the advanced noise seed.
//
// Bands are decoded low to high because each one may fold from those below it: a band with no bits
// left is filled with a scaled copy of an earlier band rather than with silence, which is why the
// loop keeps a running normalised spectrum alongside the output.
func DecodeBands(dec *rangecoder.Decoder, p *BandParams) uint32 {
	m := 1 << uint(p.LM)
	blocks := 1
	if p.ShortBlocks {
		blocks = m
	}
	channels := 1
	if p.Y != nil {
		channels = 2
	}

	total := m * BandEdges[NumBands]
	norm := make([]float32, channels*total)
	var norm2 []float32
	if channels == 2 {
		norm2 = norm[total:]
	}
	scratch := make([]float32, m*(BandEdges[NumBands]-BandEdges[NumBands-1]))

	bd := &bandDecoder{
		dec:       dec,
		cache:     defaultCache(),
		seed:      p.Seed,
		spread:    p.Spread,
		intensity: p.Intensity,
	}

	balance := p.Balance
	lowbandOffset := 0
	updateLowband := true
	dualStereo := p.DualStereo

	for i := p.Start; i < p.End; i++ {
		tell := dec.TellFrac()
		start := m * BandEdges[i]
		n := m*BandEdges[i+1] - start

		if i != p.Start {
			balance -= tell
		}
		bd.remaining = p.TotalBits - tell - 1

		b := 0
		if i <= p.CodedBands-1 {
			// Spread the running surplus over the next few bands rather than all at once.
			currBalance := balance / min(3, p.CodedBands-i)
			b = max(0, min(16383, min(bd.remaining+1, p.Pulses[i]+currBalance)))
		}

		// The folding source only moves forward while bands are getting a bit per sample or better;
		// past that, folding from a starved band would spread its damage upward.
		if start-n >= m*BandEdges[p.Start] && (updateLowband || lowbandOffset == 0) {
			lowbandOffset = i
		}

		tfChange := p.TFRes[i]
		effectiveLowband := -1
		var xcm, ycm uint32

		if lowbandOffset != 0 && (p.Spread != SpreadAggressive || blocks > 1 || tfChange < 0) {
			// Fold from just below this band, without reaching back into the band itself: repeating
			// spectral content inside one band would read as a tone rather than as noise.
			effectiveLowband = max(m*BandEdges[p.Start], m*BandEdges[lowbandOffset]-n)

			foldStart := lowbandOffset
			for {
				foldStart--
				if m*BandEdges[foldStart] <= effectiveLowband {
					break
				}
			}
			foldEnd := lowbandOffset - 1
			for {
				foldEnd++
				if m*BandEdges[foldEnd] >= effectiveLowband+n {
					break
				}
			}
			// At least one band is always folded from, even where the range comes out empty.
			for f := foldStart; ; f++ {
				xcm |= uint32(p.CollapseMasks[f*channels])
				ycm |= uint32(p.CollapseMasks[f*channels+channels-1])
				if f+1 >= foldEnd {
					break
				}
			}
		} else {
			// Nothing to fold from, so the fill comes from noise and every block gets something.
			xcm = 1<<uint(blocks) - 1
			ycm = xcm
		}

		if dualStereo && i == p.Intensity {
			// Above the intensity band the two channels share one shape, so the running spectra merge.
			dualStereo = false
			for j := m * BandEdges[p.Start]; j < start; j++ {
				norm[j] = 0.5 * (norm[j] + norm2[j])
			}
		}

		base := bandArgs{
			band: i, n: n, blocks: blocks, tfChange: tfChange,
			lm: p.LM, level: 0, gain: 1, scratch: scratch,
		}
		lowbandOf := func(buf []float32) []float32 {
			if effectiveLowband == -1 {
				return nil
			}
			return buf[effectiveLowband:]
		}

		if dualStereo {
			a := base
			a.x, a.b, a.fill = p.X[start:], b/2, int(xcm)
			a.lowband, a.lowbandOut = lowbandOf(norm), norm[start:]
			xcm = bd.decodeBand(a)

			a = base
			a.x, a.b, a.fill = p.Y[start:], b/2, int(ycm)
			a.lowband, a.lowbandOut = lowbandOf(norm2), norm2[start:]
			ycm = bd.decodeBand(a)
		} else {
			a := base
			a.x, a.b, a.fill = p.X[start:], b, int(xcm|ycm)
			if p.Y != nil {
				a.y = p.Y[start:]
			}
			a.lowband, a.lowbandOut = lowbandOf(norm), norm[start:]
			xcm = bd.decodeBand(a)
			ycm = xcm
		}

		p.CollapseMasks[i*channels] = byte(xcm)
		p.CollapseMasks[i*channels+channels-1] = byte(ycm)
		balance += p.Pulses[i] + tell
		updateLowband = b > n<<BitRes
	}

	return bd.seed
}

// blocksShift returns how many times a block count can be halved before reaching one.
func blocksShift(blocks int) int {
	if blocks < 1 {
		return 0
	}
	return bits.Len32(uint32(blocks)) - 1
}
