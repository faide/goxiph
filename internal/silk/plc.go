package silk

import (
	"math"
	"math/bits"
)

// A lost SILK frame is extrapolated rather than left silent. The last good frame's filters are kept
// and driven by its own excitation, repeated at the pitch it had, so the concealed stretch carries
// the same voice; both the pitch gain and the excitation are attenuated as the loss runs on.
//
// Adapted from silk/PLC.c.

// The concealment constants, from silk/PLC.h.
const (
	bweCoefQ16              = 64881 // 0.99
	vPitchGainStartMinQ14   = 11469 // 0.7
	vPitchGainStartMaxQ14   = 15565 // 0.95
	maxPitchLagMS           = 18
	randBufSize             = 128
	randBufMask             = randBufSize - 1
	log2InvLPCGainHighThres = 3 // 8 dB
	log2InvLPCGainLowThres  = 8 // 24 dB
	pitchDriftFacQ16        = 655

	// bweAfterLossQ16 is how far the first good frame after a loss widens its own filter, from
	// silk/define.h.
	bweAfterLossQ16 = 63570
)

// The attenuations applied per lost frame, from silk/PLC.c. The first entry is for the first loss
// and the second for every one after it, so a run of losses fades faster than it began.
var (
	harmAttQ15               = [2]int32{32440, 31130} // 0.99, 0.95
	randAttenuateVoicedQ15   = [2]int32{31130, 26214} // 0.95, 0.8
	randAttenuateUnvoicedQ15 = [2]int32{32440, 29491} // 0.99, 0.9
)

// plcState is what concealment carries between frames.
//
// It describes the last frame that arrived, not the last one played: a concealed frame updates only
// the attenuation and the seed, so however long a loss runs it keeps extrapolating from the same
// good frame.
type plcState struct {
	pitchLQ8        int32
	ltpCoefQ14      [ltpOrder]int16
	prevLPCQ12      [maxLPCOrder]int16
	prevLTPScaleQ14 int16
	prevGainQ16     [2]int32
	subframeLen     int
	subframes       int

	randSeed     int32
	randScaleQ14 int16
	rateKHz      int

	// The energy of the last concealed frame, for the fade back in once packets resume.
	concEnergy      int32
	concEnergyShift uint
	lastFrameLost   bool
}

// sync clears the state on the first frame at a new internal rate.
//
// The clearing is late rather than at the rate change because what it starts the pitch from is the
// frame length, which is not settled until a frame arrives.
func (pl *plcState) sync(s *SynthesisState, frameLength int) {
	if pl.rateKHz == s.rateKHz {
		return
	}
	*pl = plcState{
		pitchLQ8:    int32(frameLength) << 7,
		prevGainQ16: [2]int32{1 << 16, 1 << 16},
		subframeLen: 20,
		subframes:   2,
		rateKHz:     s.rateKHz,
	}
}

// update records what a good frame leaves behind for a lost one to extrapolate from.
func (pl *plcState) update(ix Indices, p Params, lags []int, ltpQ14 []int16, ltpScaleQ14 int16,
	subframes, subframeLen, lpcOrder, rateKHz int,
) {
	var gainQ14 int32
	if ix.SignalType == TypeVoiced {
		// The pitch is taken from the last subframe that carries a full period, because a shorter
		// one may not contain a pulse and its gain would say nothing about the periodicity.
		for j := 0; j*subframeLen < lags[subframes-1]; j++ {
			if j == subframes {
				break
			}
			var sum int32
			for i := range ltpOrder {
				sum += int32(ltpQ14[(subframes-1-j)*ltpOrder+i])
			}
			if sum > gainQ14 {
				gainQ14 = sum
				pl.pitchLQ8 = int32(lags[subframes-1-j]) << 8
			}
		}

		// The whole of the long-term filter collapses to its centre tap: a five-tap filter fitted to
		// one frame rings on another, and the repetition supplies the periodicity anyway.
		clear(pl.ltpCoefQ14[:])
		pl.ltpCoefQ14[ltpOrder/2] = int16(gainQ14)

		// Too weak a pitch gain and the repeat dies out at once, too strong and it turns into a
		// tone, so it is brought into a band either way.
		switch {
		case gainQ14 < vPitchGainStartMinQ14:
			scaleQ10 := (int32(vPitchGainStartMinQ14) << 10) / max(gainQ14, 1)
			for i := range ltpOrder {
				pl.ltpCoefQ14[i] = int16(smulbb(int32(pl.ltpCoefQ14[i]), scaleQ10) >> 10)
			}
		case gainQ14 > vPitchGainStartMaxQ14:
			scaleQ14 := (int32(vPitchGainStartMaxQ14) << 14) / max(gainQ14, 1)
			for i := range ltpOrder {
				pl.ltpCoefQ14[i] = int16(smulbb(int32(pl.ltpCoefQ14[i]), scaleQ14) >> 14)
			}
		}
	} else {
		// Nothing periodic to hold on to, so the repeat runs at a fixed eighteen milliseconds and
		// contributes nothing but the noise the excitation carries.
		pl.pitchLQ8 = smulbb(int32(rateKHz), 18) << 8
		clear(pl.ltpCoefQ14[:])
	}

	copy(pl.prevLPCQ12[:], p.LPCQ12[1][:lpcOrder])
	pl.prevLTPScaleQ14 = ltpScaleQ14
	pl.prevGainQ16[0] = p.GainsQ16[subframes-2]
	pl.prevGainQ16[1] = p.GainsQ16[subframes-1]
	pl.subframeLen = subframeLen
	pl.subframes = subframes
}

// conceal produces one frame's samples without a packet, and returns the pitch lag it settled on.
//
// The excitation is the last good frame's, drawn from whichever of its final two subframes had less
// energy: the quieter one is the better noise source, because a subframe carrying a pitch pulse
// would repeat that pulse out of phase.
func (pl *plcState) conceal(s *SynthesisState, out []int16,
	lossCount, prevSignalType int, firstFrameAfterReset bool,
) int {
	prevGainQ10 := [2]int32{pl.prevGainQ16[0] >> 6, pl.prevGainQ16[1] >> 6}
	if firstFrameAfterReset {
		clear(pl.prevLPCQ12[:])
	}

	scaled := make([]int16, 2*pl.subframeLen)
	for k := range 2 {
		base := (k + pl.subframes - 2) * pl.subframeLen
		for i := range pl.subframeLen {
			scaled[k*pl.subframeLen+i] = sat16(smulww(s.exc[base+i], prevGainQ10[k]) >> 8)
		}
	}
	e1, shift1 := sumSqrShift(scaled[:pl.subframeLen])
	e2, shift2 := sumSqrShift(scaled[pl.subframeLen:])

	noiseAt := max(0, pl.subframes*pl.subframeLen-randBufSize)
	if e1>>shift2 < e2>>shift1 {
		noiseAt = max(0, (pl.subframes-1)*pl.subframeLen-randBufSize)
	}
	noise := s.exc[noiseAt:]

	harmGainQ15 := harmAttQ15[min(1, lossCount)]
	randGainQ15 := randAttenuateUnvoicedQ15[min(1, lossCount)]
	if prevSignalType == TypeVoiced {
		randGainQ15 = randAttenuateVoicedQ15[min(1, lossCount)]
	}

	// Widening the filter's resonances stops it ringing on an excitation it was not fitted to.
	bwExpand16(pl.prevLPCQ12[:s.lpcOrder], bweCoefQ16)
	aQ12 := make([]int16, s.lpcOrder)
	copy(aQ12, pl.prevLPCQ12[:s.lpcOrder])

	randScaleQ14 := pl.randScaleQ14
	if lossCount == 0 {
		randScaleQ14 = 1 << 14
		if prevSignalType == TypeVoiced {
			// What the pitch filter already supplies, the noise need not.
			for i := range ltpOrder {
				randScaleQ14 -= pl.ltpCoefQ14[i]
			}
			randScaleQ14 = max(randScaleQ14, 3277)
			randScaleQ14 = int16(smulbb(int32(randScaleQ14), int32(pl.prevLTPScaleQ14)) >> 14)
		} else {
			// A sharp filter turns noise into a whistle, so the noise is quieter the more prediction
			// gain the filter has.
			invGainQ30 := inversePredGain(pl.prevLPCQ12[:s.lpcOrder])
			downQ30 := min(int32(1)<<30>>log2InvLPCGainHighThres, invGainQ30)
			downQ30 = max(int32(1)<<30>>log2InvLPCGainLowThres, downQ30)
			randGainQ15 = smulwb(downQ30<<log2InvLPCGainHighThres, randGainQ15) >> 14
		}
	}

	randSeed := pl.randSeed
	lag := int(rshiftRound(pl.pitchLQ8, 8))
	at := s.ltpMemoryLen
	subframes := len(out) / s.subframeLen

	// The long-term filter's state is recovered by running the output history back through the
	// short-term filter, the same way a good frame recovers it after the filter changes.
	whitened := make([]int16, s.ltpMemoryLen)
	start := max(0, s.ltpMemoryLen-lag-s.lpcOrder-ltpOrder/2)
	analysisFilter(whitened[start:], s.outBuf[start:], aQ12, s.ltpMemoryLen-start, s.lpcOrder)

	invGainQ30 := min(inverse32VarQ(pl.prevGainQ16[1], 46), math.MaxInt32>>1)
	ltpState := make([]int32, s.ltpMemoryLen+len(out))
	for i := start + s.lpcOrder; i < s.ltpMemoryLen; i++ {
		ltpState[i] = smulwb(invGainQ30, int32(whitened[i]))
	}

	for range subframes {
		from := at - lag + ltpOrder/2
		for i := range s.subframeLen {
			// The bias of two offsets the multiply's rounding, as in ordinary synthesis.
			pred := int32(2)
			for j := range ltpOrder {
				pred = smlawb(pred, ltpState[from+i-j], int32(pl.ltpCoefQ14[j]))
			}
			randSeed = randStep(randSeed)
			ltpState[at] = smlawb(pred, noise[int(randSeed>>25)&randBufMask], int32(randScaleQ14)) << 2
			at++
		}

		for j := range ltpOrder {
			pl.ltpCoefQ14[j] = int16(smulbb(harmGainQ15, int32(pl.ltpCoefQ14[j])) >> 15)
		}
		randScaleQ14 = int16(smulbb(int32(randScaleQ14), randGainQ15) >> 15)

		// The pitch is drifted upwards a little each subframe, which keeps a long loss from sounding
		// like a held note.
		pl.pitchLQ8 = smlawb(pl.pitchLQ8, pl.pitchLQ8, pitchDriftFacQ16)
		pl.pitchLQ8 = min(pl.pitchLQ8, smulbb(maxPitchLagMS, int32(s.rateKHz))<<8)
		lag = int(rshiftRound(pl.pitchLQ8, 8))
	}

	base := s.ltpMemoryLen - maxLPCOrder
	copy(ltpState[base:], s.lpcState[:])
	for i := range out {
		pred := int32(s.lpcOrder >> 1)
		for j := range s.lpcOrder {
			pred = smlawb(pred, ltpState[s.ltpMemoryLen+i-1-j], int32(aQ12[j]))
		}
		ltpState[s.ltpMemoryLen+i] += pred << 4
		out[i] = sat16(rshiftRound(smulww(ltpState[s.ltpMemoryLen+i], prevGainQ10[1]), 8))
	}
	copy(s.lpcState[:], ltpState[base+len(out):])

	pl.randSeed = randSeed
	pl.randScaleQ14 = randScaleQ14
	return lag
}

// glueFrames smooths the join where packets resume.
//
// A concealed frame fades, so the first good frame after one can be far louder and arrive as a
// click. Where it is, it starts at the concealed frame's level and climbs back, fast enough that an
// attack landing on that frame is not flattened.
func (pl *plcState) glueFrames(frame []int16, lossCount int) {
	if lossCount > 0 {
		pl.concEnergy, pl.concEnergyShift = sumSqrShift(frame)
		pl.lastFrameLost = true
		return
	}
	defer func() { pl.lastFrameLost = false }()
	if !pl.lastFrameLost {
		return
	}

	energy, shift := sumSqrShift(frame)
	switch {
	case shift > pl.concEnergyShift:
		pl.concEnergy >>= shift - pl.concEnergyShift
	case shift < pl.concEnergyShift:
		energy >>= pl.concEnergyShift - shift
	}
	if energy <= pl.concEnergy {
		return
	}

	lz := uint(bits.LeadingZeros32(uint32(pl.concEnergy))) - 1
	pl.concEnergy <<= lz
	energy >>= uint(max(24-int(lz), 0))

	gainQ16 := sqrtApprox(pl.concEnergy/max(energy, 1)) << 4
	// Four times the slope a straight ramp would need, so the gain is back to one well before the
	// frame ends and an onset in it is not missed.
	slopeQ16 := ((1<<16 - gainQ16) / int32(int16(len(frame)))) << 2
	for i := range frame {
		frame[i] = int16(smulwb(gainQ16, int32(frame[i])))
		gainQ16 += slopeQ16
		if gainQ16 > 1<<16 {
			break
		}
	}
}
