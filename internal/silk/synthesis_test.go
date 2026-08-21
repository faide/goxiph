package silk

import (
	"testing"

	"github.com/faide/goxiph/internal/rangecoder"
)

// TestSynthesisMatchesReference decodes whole frames to samples and compares them with libopus.
//
// This is the stage where everything before it is exercised at once, and where a decoder stops being
// checkable field by field: both filters carry state across subframes and across frames, so a single
// wrong sample propagates rather than staying put. Comparing sample for sample is the only reading
// that means anything here.
func TestSynthesisMatchesReference(t *testing.T) {
	cases := readVectors(t)
	frames, samples, voiced, interpolated := 0, 0, 0, 0

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
		syn := NewSynthesisState(c.config.SampleRateKHz, c.config.Subframes())
		subframes := c.config.Subframes()

		for _, want := range c.frames {
			mode := CodeIndependently
			if want.index > 0 {
				mode = CodeConditionally
			}
			ix := DecodeIndices(dec, c.config, &st, mode, h.VAD[want.channel][want.index], false)
			pulses := DecodePulses(dec, ix.SignalType, ix.QuantOffsetType, c.config.FrameLength())

			var p Params
			p.GainsQ16 = DequantiseGains(ix.GainIndices[:], &prevGain, mode == CodeConditionally, subframes)

			nlsf := DecodeNLSF(ix.NLSFIndices[:cb.order+1], cb)
			p.LPCQ12[1] = NLSFToLPC(nlsf, cb.order)
			p.LPCQ12[0] = p.LPCQ12[1]
			if ix.NLSFInterpolation < 4 && prevNLSF != nil {
				interpolated++
				mid := make([]int16, cb.order)
				for k := range cb.order {
					mid[k] = prevNLSF[k] + int16(int32(ix.NLSFInterpolation)*int32(nlsf[k]-prevNLSF[k])>>2)
				}
				p.LPCQ12[0] = NLSFToLPC(mid, cb.order)
			} else {
				// The reference forces no interpolation on the first frame after a reset, so the
				// index has to be neutralised or the synthesis rebuilds its filter at the wrong
				// point.
				ix.NLSFInterpolation = 4
			}
			prevNLSF = nlsf

			var lags []int
			var taps []int16
			var scale int16
			if ix.SignalType == TypeVoiced {
				voiced++
				lags = DecodePitchLags(ix.LagIndex, ix.ContourIndex, c.config.SampleRateKHz, subframes)
				taps = DecodeLTPCoefficients(ix.Periodicity, ix.LTPIndices[:], subframes)
				scale = LTPScale(ix.LTPScaleIndex)
			} else {
				lags = make([]int, subframes)
				taps = make([]int16, ltpOrder*subframes)
			}

			got := syn.Synthesise(ix, p, lags, taps, scale, pulses, subframes)
			if len(got) != len(want.samples) {
				t.Fatalf("case %d frame %d: %d samples, reference has %d",
					i, want.index, len(got), len(want.samples))
			}
			for k := range got {
				if int(got[k]) != want.samples[k] {
					t.Fatalf("case %d frame %d sample %d: %d, reference has %d",
						i, want.index, k, got[k], want.samples[k])
				}
			}
			frames++
			samples += len(got)
		}
	}

	if frames == 0 {
		t.Fatal("no frames were synthesised")
	}
	if voiced == 0 {
		t.Error("no voiced frame; the long-term filter went unexercised")
	}
	if interpolated == 0 {
		t.Error("no frame interpolated its filter, so the mid-frame rebuild went unexercised")
	}
	t.Logf("%d frames, %d samples match the reference; %d voiced, %d interpolated",
		frames, samples, voiced, interpolated)
}
