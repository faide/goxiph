package silk

import "github.com/faide/goxiph/internal/rangecoder"

// A stereo SILK frame is coded as a mid channel and a side channel rather than as left and right,
// because speech in two channels is mostly the same signal twice. The side is predicted from the mid
// with two coefficients, and only what the prediction misses is coded; often the side is not coded
// at all.
//
// Adapted from silk/stereo_decode_pred.c and silk/stereo_MS_to_LR.c.

// stereoQuantSubSteps is how finely the space between two prediction levels is divided, and
// stereoHalfSubStep is half of one such step in Q16.
const (
	stereoQuantSubSteps = 5
	stereoHalfSubStep   = 6554 // 0.5/5 in Q16, rounded as the reference rounds it
)

// stereoInterpMS is how long the predictors take to move from one frame's values to the next, in
// milliseconds. Stepping them at a frame boundary would put a click there.
const stereoInterpMS = 8

// StereoState carries the prediction and the sample history between frames.
type StereoState struct {
	prevPredQ13 [2]int32
	// mid and side each keep two samples, because the prediction reads a three-sample window and so
	// reaches back past the start of its own frame.
	mid  [2]int16
	side [2]int16
}

// DecodeStereoPrediction reads the two prediction coefficients.
//
// The pair is coded jointly at its coarsest level, because the two are correlated: a signal whose
// side is weakly predicted at one lag is usually weakly predicted at the other.
func DecodeStereoPrediction(d *rangecoder.Decoder) [2]int32 {
	var ix [2][3]int
	joint := d.DecodeICDF(stereoPredictionJointICDF[:], 8)
	ix[0][2] = joint / 5
	ix[1][2] = joint - 5*ix[0][2]

	for n := range 2 {
		ix[n][0] = d.DecodeICDF(uniform3ICDF[:], 8)
		ix[n][1] = d.DecodeICDF(uniform5ICDF[:], 8)
	}

	var pred [2]int32
	for n := range 2 {
		ix[n][0] += 3 * ix[n][2]
		low := int32(stereoPredictionLevels[ix[n][0]])
		// Half a sub-step, so the reconstructed value sits in the middle of its interval rather than
		// at the edge.
		step := smulwb(int32(stereoPredictionLevels[ix[n][0]+1])-low, stereoHalfSubStep)
		pred[n] = low + smulbb(step, int32(2*ix[n][1]+1))
	}

	// The first predictor is stored as a difference, which is what applying them wants.
	pred[0] -= pred[1]
	return pred
}

// DecodeMidOnly reads the flag saying the side channel was not coded at all.
func DecodeMidOnly(d *rangecoder.Decoder) bool {
	return d.DecodeICDF(stereoMidOnlyICDF[:], 8) != 0
}

// MidSideToLeftRight turns a decoded mid and side into left and right, in place.
//
// mid and side each hold two samples of history followed by the frame, because the prediction reads
// a three-sample window centred on each sample. The predictors are ramped rather than switched, over
// the first eight milliseconds.
func (s *StereoState) MidSideToLeftRight(mid, side []int16, predQ13 [2]int32, rateKHz, frameLength int) {
	// Bring in the previous frame's tail, and keep this frame's for the next.
	copy(mid[:2], s.mid[:])
	copy(side[:2], s.side[:])
	copy(s.mid[:], mid[frameLength:frameLength+2])
	copy(s.side[:], side[frameLength:frameLength+2])

	ramp := stereoInterpMS * rateKHz
	denomQ16 := int32(1<<16) / int32(ramp)
	delta0 := rshiftRound(smulbb(predQ13[0]-s.prevPredQ13[0], denomQ16), 16)
	delta1 := rshiftRound(smulbb(predQ13[1]-s.prevPredQ13[1], denomQ16), 16)

	pred0, pred1 := s.prevPredQ13[0], s.prevPredQ13[1]
	for n := range frameLength {
		if n < ramp {
			pred0 += delta0
			pred1 += delta1
		} else if n == ramp {
			pred0, pred1 = predQ13[0], predQ13[1]
		}

		// The first predictor works on a three-tap smoothing of the mid, which is what lets it stand
		// for a slight delay between the channels rather than only a level difference.
		smoothed := (int32(mid[n]) + int32(mid[n+2]) + int32(mid[n+1])<<1) << 9
		sum := smlawb(int32(side[n+1])<<8, smoothed, pred0)
		sum = smlawb(sum, int32(mid[n+1])<<11, pred1)
		side[n+1] = sat16(rshiftRound(sum, 8))
	}
	s.prevPredQ13 = predQ13

	for n := range frameLength {
		l := int32(mid[n+1]) + int32(side[n+1])
		r := int32(mid[n+1]) - int32(side[n+1])
		mid[n+1] = sat16(l)
		side[n+1] = sat16(r)
	}
}

// PrevPrediction returns the predictors last applied, which a frame with no coded prediction reuses.
func (s *StereoState) PrevPrediction() [2]int32 { return s.prevPredQ13 }

// BufferMono carries the two-sample history across a mono frame.
//
// A mono stream has no mid and side to combine, but it uses the same buffer and the same history,
// because the resampler is fed from one sample in rather than from the frame's start. That offset is
// the codec's alignment against CELT, and it is the same in both cases.
func (s *StereoState) BufferMono(mid []int16, frameLength int) {
	copy(mid[:2], s.mid[:])
	copy(s.mid[:], mid[frameLength:frameLength+2])
}
