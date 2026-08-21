package silk

// SILK decodes at 8, 12 or 16 kHz, and Opus delivers 48. The step up is done in two stages: an
// allpass network that doubles the rate cleanly, then a fractional interpolator that covers whatever
// ratio is left. Splitting it that way keeps the interpolator short, because it only ever has to
// bridge a factor of three or less.
//
// Adapted from silk/resampler.c, silk/resampler_private_up2_HQ.c and
// silk/resampler_private_IIR_FIR.c.

// resamplerFIROrder is the interpolator's length. The filter is symmetric, so only half its taps are
// stored and the other half are read backwards.
const resamplerFIROrder = 8

// outputRateKHz is what Opus asks SILK for, whatever rate it decoded at.
const outputRateKHz = 48

// inputDelayTo48 is how many input samples the resampler holds back, by input rate.
//
// The delay aligns SILK's output with CELT's, so that a stream switching between them does not step.
// From the decoder half of the delay matrix in silk/resampler.c.
var inputDelayTo48 = map[int]int{NarrowBand: 0, MediumBand: 4, WideBand: 7}

// Resampler converts one channel from the internal rate to 48 kHz, carrying its filter state.
type Resampler struct {
	rateKHz     int
	inputDelay  int
	ratioQ16    int32
	iir         [6]int32 // the doubling stage's allpass memory
	fir         [resamplerFIROrder]int16
	delayBuffer []int16
}

// NewResampler returns a resampler from the given internal rate to 48 kHz.
func NewResampler(rateKHz int) *Resampler {
	delay := inputDelayTo48[rateKHz]
	r := &Resampler{
		rateKHz:     rateKHz,
		inputDelay:  delay,
		delayBuffer: make([]int16, rateKHz),
	}

	// The step through the doubled signal, per output sample. It is rounded up so that the walk
	// never runs past the samples that were produced for it.
	inHz := int32(rateKHz) * 1000
	outHz := int32(outputRateKHz) * 1000
	r.ratioQ16 = ((inHz << 15) / outHz) << 2
	for smulww(r.ratioQ16, outHz) < inHz<<1 {
		r.ratioQ16++
	}
	return r
}

// clone returns an independent copy, which is what a channel appearing mid-stream starts from: it
// continues the other channel's phase rather than beginning from silence.
func (r *Resampler) clone() *Resampler {
	c := *r
	c.delayBuffer = append([]int16(nil), r.delayBuffer...)
	return &c
}

// upsample2x doubles the rate through two allpass chains, one per output phase.
//
// An allpass filter passes every frequency at full amplitude and only shifts phase, so running two
// of them at half a sample apart and interleaving their outputs inserts a sample without the ripple
// a plain filter would leave.
func (r *Resampler) upsample2x(out []int16, in []int16) {
	for k, s := range in {
		in32 := int32(s) << 10

		chain := func(state *[6]int32, base int, coefs [3]int16) int32 {
			// The first two sections take the coefficient directly; the third folds the input back
			// in, which is what a negative coefficient needs to stay stable.
			y := in32 - state[base]
			x := smulwb(y, int32(coefs[0]))
			o := state[base] + x
			state[base] = in32 + x

			y = o - state[base+1]
			x = smulwb(y, int32(coefs[1]))
			o2 := state[base+1] + x
			state[base+1] = o + x

			y = o2 - state[base+2]
			x = smlawb(y, y, int32(coefs[2]))
			o = state[base+2] + x
			state[base+2] = o2 + x
			return o
		}

		out[2*k] = sat16(rshiftRound(chain(&r.iir, 0, resamplerUp2Even), 10))
		out[2*k+1] = sat16(rshiftRound(chain(&r.iir, 3, resamplerUp2Odd), 10))
	}
}

// interpolate walks the doubled signal at the output rate, reading a fractional filter at each step.
//
// The filter is symmetric about its centre, so the second half of its taps is the first half read
// backwards from the complementary offset. That halves the table.
func interpolate(out []int16, buf []int16, maxIndexQ16, incrementQ16 int32) int {
	n := 0
	for indexQ16 := int32(0); indexQ16 < maxIndexQ16; indexQ16 += incrementQ16 {
		table := smulwb(indexQ16&0xFFFF, 12)
		at := buf[indexQ16>>16:]
		mirror := 11 - table

		acc := int32(0)
		for j := range 4 {
			acc += int32(at[j]) * int32(resamplerFracFIR[table][j])
		}
		for j := range 4 {
			acc += int32(at[4+j]) * int32(resamplerFracFIR[mirror][3-j])
		}
		out[n] = sat16(rshiftRound(acc, 15))
		n++
	}
	return n
}

// resampleBatch doubles then interpolates one run of input, carrying the interpolator's overlap.
func (r *Resampler) resampleBatch(out []int16, in []int16) int {
	buf := make([]int16, resamplerFIROrder+2*len(in))
	copy(buf, r.fir[:])
	r.upsample2x(buf[resamplerFIROrder:], in)

	n := interpolate(out, buf, int32(len(in))<<17, r.ratioQ16)
	copy(r.fir[:], buf[len(in)<<1:])
	return n
}

// Resample converts a frame to 48 kHz. It needs at least one millisecond of input.
//
// The first millisecond comes from the delay buffer rather than from the caller, which is how the
// alignment against CELT is held without a separate delay line downstream.
func (r *Resampler) Resample(in []int16) []int16 {
	if len(in) < r.rateKHz {
		return nil
	}

	out := make([]int16, len(in)*outputRateKHz/r.rateKHz)
	head := r.rateKHz - r.inputDelay
	copy(r.delayBuffer[r.inputDelay:], in[:head])

	// The second batch stops one millisecond short of the end: those samples are the next frame's
	// head, and go to the delay buffer instead.
	n := r.resampleBatch(out, r.delayBuffer)
	n += r.resampleBatch(out[n:], in[head:head+len(in)-r.rateKHz])

	copy(r.delayBuffer, in[len(in)-r.inputDelay:])
	return out[:n]
}
