package silk

import (
	"fmt"

	"github.com/faide/goxiph/internal/rangecoder"
)

// Decoder decodes SILK payloads, carrying the state that spans frames and packets.
//
// A SILK stream is not a sequence of independent frames: gains, the spectrum, the pitch and both
// filters all refer back. A decoder that has missed a frame cannot resume cleanly, which is why the
// state lives here rather than being rebuilt per call.
type Decoder struct {
	config Config

	frames   frameState
	prevGain int32
	prevNLSF []int16
	syn      *SynthesisState
	resample *Resampler

	// lastSample is the final sample of the previous frame. The resampler is fed starting from it
	// rather than from the current frame, which delays the output by one sample at the internal
	// rate. That delay is what aligns SILK against CELT, and a decoder without it runs early.
	lastSample int16
}

// NewDecoder returns a decoder for one configuration.
func NewDecoder(c Config) (*Decoder, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.Channels != 1 {
		// Stereo needs the mid/side prediction, which is not written.
		return nil, fmt.Errorf("silk: %d channels is not supported yet", c.Channels)
	}
	d := &Decoder{config: c}
	d.reset()
	return d, nil
}

// Config returns the configuration this decoder was built for.
func (d *Decoder) Config() Config { return d.config }

// reset clears everything that carries between frames.
func (d *Decoder) reset() {
	d.frames = frameState{}
	d.prevGain = 0
	d.prevNLSF = nil
	d.syn = NewSynthesisState(d.config.SampleRateKHz, d.config.Subframes())
	d.resample = NewResampler(d.config.SampleRateKHz)
	d.lastSample = 0
}

// Decode reads one SILK payload of the given duration and returns its samples at 48 kHz.
//
// The duration is a property of the packet rather than of the stream, and it may change from one
// packet to the next without the decoder resetting: a shorter packet still predicts from what came
// before it. Only a change of internal rate starts afresh, which is what NewDecoder is for.
//
// The range decoder is shared with whatever follows in the packet, so it is advanced exactly as far
// as SILK's own symbols reach and no further. In a hybrid packet CELT reads from where this stops.
func (d *Decoder) Decode(dec *rangecoder.Decoder, frameMS int) ([]int16, error) {
	d.config.FrameMS = frameMS
	if err := d.config.Validate(); err != nil {
		return nil, err
	}

	header, err := DecodeHeader(dec, d.config)
	if err != nil {
		return nil, err
	}

	frames := d.config.FramesPerPacket()
	subframes := d.config.Subframes()

	// Redundant copies of earlier frames come first. They are not played, but their symbols are
	// still in the stream and have to be read past.
	//
	// Nothing in the corpus reaches this: opusenc offers no way to turn in-band redundancy on, so
	// the path is written from the reference and unverified against it.
	if err := d.skipRedundant(dec, header, frames); err != nil {
		return nil, err
	}

	out := make([]int16, 0, frames*d.config.FrameLength()*outputRateKHz/d.config.SampleRateKHz)
	for i := range frames {
		// Every frame but the first refers back to its predecessor. Whether the packet also carries
		// redundancy does not enter into it: that only decides how the redundant frames themselves
		// are coded, which happens above and is discarded.
		mode := CodeIndependently
		if i > 0 {
			mode = CodeConditionally
		}
		pcm := d.decodeFrame(dec, mode, header.VAD[0][i], subframes)
		out = append(out, d.resampleFrame(pcm)...)
	}
	return out, nil
}

// resampleFrame lifts a frame to the output rate, one sample behind.
//
// The sample carried over from the previous frame is not padding: it is what the reference feeds its
// resampler in place of the frame's own last sample, which it holds back until next time.
func (d *Decoder) resampleFrame(pcm []int16) []int16 {
	delayed := make([]int16, len(pcm))
	delayed[0] = d.lastSample
	copy(delayed[1:], pcm[:len(pcm)-1])
	d.lastSample = pcm[len(pcm)-1]
	return d.resample.Resample(delayed)
}

// skipRedundant reads past the low-bitrate redundancy a packet may carry.
func (d *Decoder) skipRedundant(dec *rangecoder.Decoder, h Header, frames int) error {
	if !h.LBRR[0] {
		return nil
	}
	for i := range frames {
		if !h.LBRRFrame[0][i] {
			continue
		}
		mode := CodeIndependently
		if i > 0 && h.LBRRFrame[0][i-1] {
			mode = CodeConditionally
		}
		// A redundant frame is always of active speech, so it reads from the active distribution
		// whatever the activity flags say.
		ix := DecodeIndices(dec, d.config, &d.frames, mode, true, true)
		DecodePulses(dec, ix.SignalType, ix.QuantOffsetType, d.config.FrameLength())
	}
	return nil
}

// decodeFrame reads one frame and synthesises it at the internal rate.
func (d *Decoder) decodeFrame(dec *rangecoder.Decoder, mode CodingMode, active bool, subframes int) []int16 {
	cb := codebookFor(d.config.SampleRateKHz)

	ix := DecodeIndices(dec, d.config, &d.frames, mode, active, false)
	pulses := DecodePulses(dec, ix.SignalType, ix.QuantOffsetType, d.config.FrameLength())

	var p Params
	p.GainsQ16 = DequantiseGains(ix.GainIndices[:], &d.prevGain, mode == CodeConditionally, subframes)

	nlsf := DecodeNLSF(ix.NLSFIndices[:cb.order+1], cb)
	p.LPCQ12[1] = NLSFToLPC(nlsf, cb.order)
	p.LPCQ12[0] = p.LPCQ12[1]

	if ix.NLSFInterpolation < 4 && d.prevNLSF != nil {
		// The frame's first half runs a filter part way between this frame's spectrum and the last.
		mid := make([]int16, cb.order)
		for k := range cb.order {
			mid[k] = d.prevNLSF[k] + int16(int32(ix.NLSFInterpolation)*int32(nlsf[k]-d.prevNLSF[k])>>2)
		}
		p.LPCQ12[0] = NLSFToLPC(mid, cb.order)
	} else {
		// With nothing to interpolate from, the synthesis must not rebuild its filter mid-frame
		// either, which it decides from this same value.
		ix.NLSFInterpolation = 4
	}
	d.prevNLSF = nlsf

	lags := make([]int, subframes)
	taps := make([]int16, ltpOrder*subframes)
	var scale int16
	if ix.SignalType == TypeVoiced {
		lags = DecodePitchLags(ix.LagIndex, ix.ContourIndex, d.config.SampleRateKHz, subframes)
		taps = DecodeLTPCoefficients(ix.Periodicity, ix.LTPIndices[:], subframes)
		scale = LTPScale(ix.LTPScaleIndex)
	}

	return d.syn.Synthesise(ix, p, lags, taps, scale, pulses, subframes)
}
