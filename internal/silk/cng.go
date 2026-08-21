package silk

// Concealment fades, so a long silence between talkspurts would fade to nothing and the line would
// sound dead. Comfort noise fills it: the spectrum and level of the last inactive frames are kept
// and re-synthesised over whatever concealment produced, so the background stays where it was.
//
// Adapted from silk/CNG.c.

// The smoothing rates and the excitation buffer's index mask, from silk/define.h.
const (
	cngGainSmoothQ16 = 4634  // 0.25^(1/4)
	cngNLSFSmoothQ16 = 16348 // 0.25
	cngBufMaskMax    = 255
)

// cngState is what comfort noise carries between frames.
type cngState struct {
	smoothNLSFQ15 [maxLPCOrder]int16
	excQ14        [maxFrameLength]int32
	smoothGainQ16 int32
	randSeed      int32
	synthState    [maxLPCOrder]int32
	rateKHz       int
}

// sync clears the state on the first frame at a new internal rate, spreading the spectrum evenly and
// silencing the level.
func (c *cngState) sync(s *SynthesisState, lpcOrder int) {
	if c.rateKHz == s.rateKHz {
		return
	}
	*c = cngState{randSeed: 3176576, rateKHz: s.rateKHz}

	step := int32(32767) / int32(lpcOrder+1)
	acc := int32(0)
	for i := range lpcOrder {
		acc += step
		c.smoothNLSFQ15[i] = int16(acc)
	}
}

// update folds one inactive frame into the running estimate of the background.
//
// Only frames with no voice activity contribute, which is the whole point: the estimate has to be of
// the room, not of the speech.
func (c *cngState) update(nlsfQ15 []int16, p Params, exc []int32, subframes, subframeLen, lpcOrder int) {
	for i := range lpcOrder {
		c.smoothNLSFQ15[i] += int16(smulwb(int32(nlsfQ15[i]-c.smoothNLSFQ15[i]), cngNLSFSmoothQ16))
	}

	// The loudest subframe is the one kept, so a buffer built over several frames is not dominated
	// by whichever happened to fall quiet.
	loudest := 0
	var maxGainQ16 int32
	for i := range subframes {
		if p.GainsQ16[i] > maxGainQ16 {
			maxGainQ16, loudest = p.GainsQ16[i], i
		}
	}
	copy(c.excQ14[subframeLen:], c.excQ14[:(subframes-1)*subframeLen])
	copy(c.excQ14[:subframeLen], exc[loudest*subframeLen:])

	for i := range subframes {
		c.smoothGainQ16 += smulwb(p.GainsQ16[i]-c.smoothGainQ16, cngGainSmoothQ16)
	}
}

// generate adds comfort noise to a concealed frame.
func (c *cngState) generate(frame []int16, lpcOrder int) {
	// The index mask is the largest power of two the frame can hold, so the noise repeats no more
	// often than it has to.
	mask := int32(cngBufMaskMax)
	for mask > int32(len(frame)) {
		mask >>= 1
	}

	sig := make([]int32, len(frame)+maxLPCOrder)
	copy(sig, c.synthState[:])
	for i := range frame {
		c.randSeed = randStep(c.randSeed)
		sig[maxLPCOrder+i] = int32(sat16(smulww(c.excQ14[int(c.randSeed>>24)&int(mask)], c.smoothGainQ16>>4)))
	}

	aQ12 := NLSFToLPC(c.smoothNLSFQ15[:lpcOrder], lpcOrder)
	for i := range frame {
		// The same bias as ordinary synthesis, half the filter order.
		sumQ6 := int32(lpcOrder >> 1)
		for j := range lpcOrder {
			sumQ6 = smlawb(sumQ6, sig[maxLPCOrder+i-1-j], int32(aQ12[j]))
		}
		sig[maxLPCOrder+i] += sumQ6 << 4
		frame[i] = addSat16(frame[i], int16(rshiftRound(sumQ6, 6)))
	}
	copy(c.synthState[:], sig[len(frame):])
}

// silence clears the synthesis filter's memory, which a good frame leaves behind it.
func (c *cngState) silence(lpcOrder int) {
	clear(c.synthState[:lpcOrder])
}
