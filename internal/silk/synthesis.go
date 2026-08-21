package silk

// Synthesis runs the two prediction filters in reverse. The excitation is fed through the long-term
// filter, which puts the pitch periodicity back, and then through the short-term filter, which puts
// the spectral envelope back. Both filters carry state across subframes and across frames, so this
// stage is where a decoder's history matters most: an error here does not stay in its own frame.
//
// Adapted from silk/decode_core.c.

// Quantisation offsets, from silk/tables_other.c. The excitation is biased away from zero by one of
// these, which keeps a run of zero pulses from reading as complete silence.
var quantisationOffsetsQ10 = [2][2]int32{
	{100, 240}, // inactive or unvoiced
	{32, 100},  // voiced
}

// quantLevelAdjustQ10 is the offset applied to a non-zero pulse, mirroring the encoder's rounding.
const quantLevelAdjustQ10 = 80

// ltpMemoryMS is how much past output the long-term filter can reach back into.
const ltpMemoryMS = 20

// maxLPCOrder is the widest short-term filter, which fixes the state buffer's size whatever the
// current order is.
const maxLPCOrder = 16

// maxFrameLength is the longest frame at the widest internal rate, which fixes the excitation
// buffer's size whatever the current frame's is.
const maxFrameLength = 20 * WideBand

// SynthesisState is what a channel carries between frames.
type SynthesisState struct {
	// outBuf holds recent output, which the long-term filter re-whitens to recover its own state.
	outBuf []int16
	// lpcState is the short-term filter's memory, in Q14.
	lpcState [maxLPCOrder]int32
	// prevGainQ16 is the last subframe's gain, used to rescale the state when the gain moves.
	prevGainQ16 int32
	// exc holds the last decoded frame's excitation, which concealment draws its noise from. A
	// shorter frame overwrites only its own length, so the tail is whatever an earlier one left —
	// which is what the reference reads too.
	exc [maxFrameLength]int32

	rateKHz      int
	subframeLen  int
	ltpMemoryLen int
	lpcOrder     int
}

// NewSynthesisState returns a state for one channel at the given rate and frame layout.
func NewSynthesisState(rateKHz, subframes int) *SynthesisState {
	order := 10
	if rateKHz == WideBand {
		order = maxLPCOrder
	}
	subframeLen := SubframeLengthMS * rateKHz

	return &SynthesisState{
		outBuf:       make([]int16, ltpMemoryMS*rateKHz+2*subframeLen),
		prevGainQ16:  65536,
		rateKHz:      rateKHz,
		subframeLen:  subframeLen,
		ltpMemoryLen: ltpMemoryMS * rateKHz,
		lpcOrder:     order,
	}
}

// excitation expands the coded pulses into the signal the filters are driven with.
//
// Each pulse is pulled back towards zero by the quantiser's rounding offset, pushed away from it by
// the signal-type offset, and then given a sign from a generator seeded by the frame. That last
// step is why a run of zeros does not sound like a gap: it becomes low-level noise instead.
func excitation(pulses []int, signalType, quantOffsetType, seed int) []int32 {
	out := make([]int32, len(pulses))
	offsetQ10 := quantisationOffsetsQ10[signalType>>1][quantOffsetType]

	rand := int32(seed)
	for i, p := range pulses {
		rand = randStep(rand)

		v := int32(p) << 14
		switch {
		case v > 0:
			v -= quantLevelAdjustQ10 << 4
		case v < 0:
			v += quantLevelAdjustQ10 << 4
		}
		v += offsetQ10 << 4

		if rand < 0 {
			v = -v
		}
		out[i] = v

		// The pulse feeds back into the generator, so the noise follows the signal rather than
		// repeating on a fixed cycle.
		rand += int32(p)
	}
	return out
}

// analysisFilter runs the short-term filter forwards, recovering the excitation that produced a
// stretch of output. It is what re-establishes the long-term filter's state after the filter itself
// has changed.
func analysisFilter(out, in []int16, aQ12 []int16, length, order int) {
	for ix := order; ix < length; ix++ {
		var acc int32
		for j := range order {
			acc += int32(in[ix-1-j]) * int32(aQ12[j])
		}
		acc = int32(in[ix])<<12 - acc
		out[ix] = sat16(rshiftRound(acc, 12))
	}
}

// Synthesise turns one frame's excitation and parameters into samples.
//
// lossCount and prevSignalType describe what came before, because the first frame after a run of
// concealment is bridged rather than started clean: see the voiced-to-unvoiced case below.
func (s *SynthesisState) Synthesise(ix Indices, p Params, lags []int, ltpQ14 []int16,
	ltpScaleQ14 int16, pulses []int, subframes int, lossCount, prevSignalType, lagPrev int,
) []int16 {
	frameLength := subframes * s.subframeLen
	out := make([]int16, frameLength)

	exc := excitation(pulses, ix.SignalType, ix.QuantOffsetType, ix.Seed)
	copy(s.exc[:], exc)
	interpolated := ix.NLSFInterpolation < 4

	// The long-term state runs to twice a frame so that a subframe can reach a full lag behind its
	// own start without falling off the front.
	ltpState := make([]int32, 2*frameLength+s.ltpMemoryLen)
	whitened := make([]int16, len(s.outBuf))
	ltpIndex := s.ltpMemoryLen

	lpc := make([]int32, s.subframeLen+maxLPCOrder)
	copy(lpc, s.lpcState[:])

	for k := range subframes {
		// The frame's two halves may use different filters, where the first was interpolated.
		aQ12 := p.LPCQ12[k>>1]
		bQ14 := ltpQ14[k*ltpOrder:]
		signalType := ix.SignalType

		// A concealed voiced stretch followed by an unvoiced frame drops the periodicity all at
		// once, which is heard as a click. The frame's first half is bridged instead: it keeps the
		// concealed pitch at a quarter gain and only then lets the unvoiced coding take over.
		if lossCount > 0 && prevSignalType == TypeVoiced && ix.SignalType != TypeVoiced &&
			k < MaxSubframesInFrame/2 {
			bQ14 = make([]int16, ltpOrder)
			bQ14[ltpOrder/2] = 1 << 12
			signalType = TypeVoiced
			lags[k] = lagPrev
		}

		gainQ10 := p.GainsQ16[k] >> 6
		invGainQ31 := inverse32VarQ(p.GainsQ16[k], 47)

		// A gain change rescales the filter state, so that the state means the same thing relative
		// to the new gain as it did to the old.
		gainAdjQ16 := int32(1) << 16
		if p.GainsQ16[k] != s.prevGainQ16 {
			gainAdjQ16 = div32VarQ(s.prevGainQ16, p.GainsQ16[k], 16)
			for i := range maxLPCOrder {
				lpc[i] = smulww(gainAdjQ16, lpc[i])
			}
		}
		s.prevGainQ16 = p.GainsQ16[k]

		var lag int
		if signalType == TypeVoiced {
			lag = lags[k]

			// Where the short-term filter has just changed, the long-term state has to be rebuilt
			// through the new one: the state it holds was whitened by the old filter and means
			// something different now.
			if k == 0 || (k == 2 && interpolated) {
				start := s.ltpMemoryLen - lag - s.lpcOrder - ltpOrder/2
				if start < 0 {
					start = 0
				}
				if k == 2 {
					copy(s.outBuf[s.ltpMemoryLen:], out[:2*s.subframeLen])
				}
				analysisFilter(whitened[start:], s.outBuf[start+k*s.subframeLen:],
					aQ12, s.ltpMemoryLen-start, s.lpcOrder)

				if k == 0 {
					// Only the first subframe scales down what it inherits, which is what limits how
					// far a lost packet can propagate.
					invGainQ31 = smulwb(invGainQ31, int32(ltpScaleQ14)) << 2
				}
				for i := 0; i < lag+ltpOrder/2; i++ {
					ltpState[ltpIndex-i-1] = smulwb(invGainQ31, int32(whitened[s.ltpMemoryLen-i-1]))
				}
			} else if gainAdjQ16 != 1<<16 {
				for i := 0; i < lag+ltpOrder/2; i++ {
					ltpState[ltpIndex-i-1] = smulww(gainAdjQ16, ltpState[ltpIndex-i-1])
				}
			}
		}

		residual := exc[k*s.subframeLen : (k+1)*s.subframeLen]
		if signalType == TypeVoiced {
			predicted := make([]int32, s.subframeLen)
			at := ltpIndex - lag + ltpOrder/2
			for i := range s.subframeLen {
				// The bias of two offsets the multiply's rounding, which always goes towards
				// negative infinity and would otherwise pull the whole filter down.
				pred := int32(2)
				for j := range ltpOrder {
					pred = smlawb(pred, ltpState[at+i-j], int32(bQ14[j]))
				}
				predicted[i] = residual[i] + pred<<1
				ltpState[ltpIndex] = predicted[i] << 1
				ltpIndex++
			}
			residual = predicted
		}

		for i := range s.subframeLen {
			// The same bias, here half the filter order.
			pred := int32(s.lpcOrder >> 1)
			for j := range s.lpcOrder {
				pred = smlawb(pred, lpc[maxLPCOrder+i-1-j], int32(aQ12[j]))
			}
			lpc[maxLPCOrder+i] = residual[i] + pred<<4
			out[k*s.subframeLen+i] = sat16(rshiftRound(smulww(lpc[maxLPCOrder+i], gainQ10), 8))
		}
		copy(lpc, lpc[s.subframeLen:s.subframeLen+maxLPCOrder])
	}

	copy(s.lpcState[:], lpc[:maxLPCOrder])
	s.pushOutput(out)
	return out
}

// pushOutput keeps the tail of a frame for the next one's long-term filter to reach back into.
//
// Concealment produces a frame the same way and needs the same history behind it, so this is
// separate from the synthesis that usually precedes it.
func (s *SynthesisState) pushOutput(out []int16) {
	moved := s.ltpMemoryLen - len(out)
	copy(s.outBuf, s.outBuf[len(out):len(out)+moved])
	copy(s.outBuf[moved:], out)
}
