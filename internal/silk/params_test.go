package silk

import (
	"testing"

	"github.com/faide/goxiph/internal/rangecoder"
)

// TestParametersMatchReference checks the expansion from indices to gains and filter coefficients.
//
// This is the first stage whose output is not read straight from the bitstream: it is arithmetic on
// what was read, in fixed point, and the arithmetic is part of the format. The decoder's own state
// feeds back into it frame by frame, so an error here does not stay local.
func TestParametersMatchReference(t *testing.T) {
	cases := readVectors(t)
	frames, interpolated, voicedFrames := 0, 0, 0

	for i, c := range cases {
		dec := rangecoder.NewDecoder(c.payload)
		h, err := DecodeHeader(dec, c.config)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if h.LBRR[0] || h.LBRR[1] {
			continue
		}

		var st frameState
		var prevGain int32
		var prevNLSF []int16
		cb := codebookFor(c.config.SampleRateKHz)

		for _, want := range c.frames {
			mode := CodeIndependently
			if want.index > 0 {
				mode = CodeConditionally
			}
			ix := DecodeIndices(dec, c.config, &st, mode, h.VAD[want.channel][want.index], false)
			DecodePulses(dec, ix.SignalType, ix.QuantOffsetType, c.config.FrameLength())

			gains := DequantiseGains(ix.GainIndices[:], &prevGain,
				mode == CodeConditionally, c.config.Subframes())
			for k, w := range want.gainsQ16[:c.config.Subframes()] {
				if int(gains[k]) != w {
					t.Fatalf("case %d frame %d subframe %d: gain %d, reference has %d",
						i, want.index, k, gains[k], w)
				}
			}

			nlsf := DecodeNLSF(ix.NLSFIndices[:cb.order+1], cb)
			lpc1 := NLSFToLPC(nlsf, cb.order)
			for k, w := range want.lpc1 {
				if int(lpc1[k]) != w {
					t.Fatalf("case %d frame %d coefficient %d: filter %d, reference has %d",
						i, want.index, k, lpc1[k], w)
				}
			}

			// The frame's first half may be interpolated towards the previous frame's spectrum,
			// which is the one place a filter depends on more than its own frame.
			lpc0 := lpc1
			if ix.NLSFInterpolation < 4 && prevNLSF != nil {
				interpolated++
				mid := make([]int16, cb.order)
				for k := range cb.order {
					mid[k] = prevNLSF[k] + int16(int32(ix.NLSFInterpolation)*
						int32(nlsf[k]-prevNLSF[k])>>2)
				}
				lpc0 = NLSFToLPC(mid, cb.order)
			}
			for k, w := range want.lpc0 {
				if int(lpc0[k]) != w {
					t.Fatalf("case %d frame %d coefficient %d: first-half filter %d, reference has %d",
						i, want.index, k, lpc0[k], w)
				}
			}

			if ix.SignalType == TypeVoiced {
				voicedFrames++
				lags := DecodePitchLags(ix.LagIndex, ix.ContourIndex,
					c.config.SampleRateKHz, c.config.Subframes())
				for k, w := range want.pitchLags[:c.config.Subframes()] {
					if lags[k] != w {
						t.Fatalf("case %d frame %d subframe %d: pitch lag %d, reference has %d",
							i, want.index, k, lags[k], w)
					}
				}

				taps := DecodeLTPCoefficients(ix.Periodicity, ix.LTPIndices[:], c.config.Subframes())
				for k, w := range want.ltpCoefQ14 {
					if int(taps[k]) != w {
						t.Fatalf("case %d frame %d tap %d: long-term coefficient %d, reference has %d",
							i, want.index, k, taps[k], w)
					}
				}

				if got := int(LTPScale(ix.LTPScaleIndex)); got != want.ltpScaleQ14 {
					t.Fatalf("case %d frame %d: long-term scale %d, reference has %d",
						i, want.index, got, want.ltpScaleQ14)
				}
			}

			prevNLSF = nlsf
			frames++
		}
	}

	if frames == 0 {
		t.Fatal("no frames were checked")
	}
	if interpolated == 0 {
		t.Error("no frame interpolated its spectrum; that path went untested")
	}
	t.Logf("%d frames match on gains, filter coefficients and pitch; %d interpolated, %d voiced",
		frames, interpolated, voicedFrames)
}

// TestGainFloorBoundsADrop covers a path no corpus can reach.
//
// The decoder refuses to let a frame's first gain index fall more than sixteen steps below the
// previous frame's. An encoder never asks it to: its own quantiser clamps that index to four steps
// below, so the floor is only reachable from a stream that has been corrupted or crafted. That makes
// it the kind of guard that rots unnoticed, because every real stream passes either way.
func TestGainFloorBoundsADrop(t *testing.T) {
	// One subframe, so the reading is of the first gain alone and not of the deltas after it.
	prev := int32(40)
	gains := DequantiseGains([]int{0}, &prev, false, 1)

	if prev != 24 {
		t.Errorf("an index of zero against a previous 40 settled at %d, want the floor at 24", prev)
	}
	if gains[0] == 0 {
		t.Error("the first gain came out as silence")
	}

	// Within sixteen steps the request stands, so the floor is not clamping everything.
	prev = 40
	DequantiseGains([]int{30}, &prev, false, 1)
	if prev != 30 {
		t.Errorf("a drop of ten steps settled at %d, want 30", prev)
	}
}

// TestGainStepsDoubleNearTheTop covers the other branch of the delta rule: past a threshold each
// step counts double, so the highest gains stay reachable within the same number of symbols.
func TestGainStepsDoubleNearTheTop(t *testing.T) {
	// The threshold sits eight steps above the previous index, so from zero a delta of twenty
	// crosses it and a delta of ten does not.
	crossed := int32(0)
	DequantiseGains([]int{20}, &crossed, true, 1)

	plain := int32(0)
	DequantiseGains([]int{10}, &plain, true, 1)

	if crossed <= 20+minDeltaGainQuant {
		t.Errorf("a delta past the threshold reached %d, no further than its face value", crossed)
	}
	if plain != 10+minDeltaGainQuant {
		t.Errorf("a delta below the threshold reached %d, want %d", plain, 10+minDeltaGainQuant)
	}
}

// TestResidualDeadZone pins the offset applied either side of zero.
//
// The corpus cannot see it: a residual is scaled down so far afterwards that a change of one in the
// offset vanishes before it reaches a filter coefficient. It still has to be right, so it is checked
// where its effect survives.
func TestResidualDeadZone(t *testing.T) {
	// The narrow-band step. A step of one in Q16 would not do: the multiply takes only the low
	// sixteen bits of it, and 1<<16 has none set.
	const step = 11796
	pred := make([]uint8, 4)

	// The offset is written out rather than taken from the constant under test, which would make
	// the comparison agree with itself whatever the constant said. It is a tenth in Q10.
	const deadZone = 102
	if nlsfQuantLevelAdj != deadZone {
		t.Fatalf("the dead zone is %d, want %d, which is a tenth in Q10", nlsfQuantLevelAdj, deadZone)
	}

	scaled := func(v int32) int16 { return int16(smlawb(0, v, step)) }
	got := residualDequant([]int{2, -2, 1, 0}, pred, step, 4)
	want := []int16{
		scaled(2<<10 - deadZone),
		scaled(-(2 << 10) + deadZone),
		scaled(1<<10 - deadZone),
		0,
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("residual %d is %d, want %d", i, got[i], want[i])
		}
	}

	// Without the dead zone a residual of one step would scale to a larger value, so the offset is
	// doing something rather than rounding away.
	if scaled(1<<10) <= scaled(1<<10-deadZone) {
		t.Error("the dead zone made no difference at this step size; the test proves nothing")
	}
}
