package silk

import "github.com/faide/goxiph/internal/rangecoder"

// Decoder decodes SILK payloads, carrying the state that spans frames and packets.
//
// A SILK stream is not a sequence of independent frames: gains, the spectrum, the pitch and both
// filters all refer back. A decoder that has missed a frame cannot resume cleanly, which is why the
// state lives here rather than being rebuilt per call.
type Decoder struct {
	config Config

	// One set of per-channel state. In stereo these are the mid and side channels, not left and
	// right: the conversion happens after both are synthesised.
	ch     [2]channelState
	stereo StereoState
	// prevMidOnly says the previous frame coded no side channel, which is what makes the next one
	// that does start from a cleared side decoder.
	prevMidOnly bool
	resample    [2]*Resampler
}

// channelState is everything one coded channel carries between frames.
type channelState struct {
	frames   frameState
	prevGain int32
	prevNLSF []int16
	syn      *SynthesisState
}

// NewDecoder returns a decoder for one configuration.
func NewDecoder(c Config) (*Decoder, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	d := &Decoder{config: c}
	d.reset()
	return d, nil
}

// Config returns the configuration this decoder was built for.
func (d *Decoder) Config() Config { return d.config }

// SetRate reconfigures the decoder for a new internal rate.
//
// A rate change is not a fresh start. The filters and the resampler are cleared, because their state
// describes a signal at the old rate and means nothing at the new one; the gain index restarts from
// a middle value rather than from silence. But what the entropy decoding refers back to — the
// previous frame's signal type and pitch lag — is kept, because the next frame's symbols are coded
// against it whatever the rate did.
func (d *Decoder) SetRate(rateKHz int) {
	if rateKHz == d.config.SampleRateKHz {
		return
	}
	d.config.SampleRateKHz = rateKHz

	for c := range d.config.Channels {
		// The entropy state in d.ch[c].frames survives; everything else is rebuilt.
		d.ch[c].syn = NewSynthesisState(rateKHz, d.config.Subframes())
		d.ch[c].prevNLSF = nil
		// From silk/decoder_set_fs.c: the gain index restarts at ten, not at zero.
		d.ch[c].prevGain = gainIndexAfterRateChange
		d.resample[c] = NewResampler(rateKHz)
	}
}

// gainIndexAfterRateChange is where the gain index restarts when the internal rate changes.
const gainIndexAfterRateChange = 10

// reset clears everything that carries between frames.
func (d *Decoder) reset() {
	for c := range d.config.Channels {
		d.ch[c] = channelState{syn: NewSynthesisState(d.config.SampleRateKHz, d.config.Subframes())}
		d.resample[c] = NewResampler(d.config.SampleRateKHz)
	}
	d.stereo = StereoState{}
	d.prevMidOnly = false
}

// resetSide clears the side channel, which a frame that resumes coding one starts from.
//
// The side decoder's state describes a signal that was not coded for however long the frame-only
// stretch lasted, so carrying it forward would predict from something that is not there.
func (d *Decoder) resetSide() {
	d.ch[1] = channelState{syn: NewSynthesisState(d.config.SampleRateKHz, d.config.Subframes())}
	d.ch[1].frames.prevLagIndex = 100
	d.ch[1].prevGain = 10
}

// Decode reads one SILK payload of the given duration and returns its samples at 48 kHz, one slice
// per channel.
//
// The duration is a property of the packet rather than of the stream, and it may change from one
// packet to the next without the decoder resetting: a shorter packet still predicts from what came
// before it. Only a change of internal rate starts afresh, which is what NewDecoder is for.
//
// The range decoder is shared with whatever follows in the packet, so it is advanced exactly as far
// as SILK's own symbols reach and no further. In a hybrid packet CELT reads from where this stops.
func (d *Decoder) Decode(dec *rangecoder.Decoder, frameMS int) ([][]int16, error) {
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
	stereo := d.config.Channels == 2

	// Redundant copies of earlier frames come first. They are not played, but their symbols are
	// still in the stream and have to be read past.
	//
	// Nothing in the corpus reaches this: opusenc offers no way to turn in-band redundancy on, so
	// the path is written from the reference and unverified against it.
	d.skipRedundant(dec, header, frames, subframes, stereo)

	out := make([][]int16, d.config.Channels)
	for i := range frames {
		// The prediction and the side-channel flag come before either channel's frame.
		pred := d.stereo.PrevPrediction()
		midOnly := false
		if stereo {
			pred = DecodeStereoPrediction(dec)
			// The flag is only coded where the side channel has no activity of its own to declare.
			if !header.VAD[1][i] {
				midOnly = DecodeMidOnly(dec)
			}
		}
		if stereo && !midOnly && d.prevMidOnly {
			d.resetSide()
		}

		mode := CodeIndependently
		if i > 0 {
			mode = CodeConditionally
		}

		length := d.config.FrameLength()
		// Two samples of history sit in front of each frame, because the stereo prediction reads a
		// window that reaches back past the frame's start.
		mid := make([]int16, length+2)
		side := make([]int16, length+2)
		copy(mid[2:], d.decodeFrame(dec, 0, mode, header.VAD[0][i], subframes))
		if stereo && !midOnly {
			copy(side[2:], d.decodeFrame(dec, 1, mode, header.VAD[1][i], subframes))
		}

		if stereo {
			d.stereo.MidSideToLeftRight(mid, side, pred, d.config.SampleRateKHz, length)
			d.prevMidOnly = midOnly
		} else {
			d.stereo.BufferMono(mid, length)
		}

		// One sample in, not at the frame's start: the buffer's first entries are the previous
		// frame's tail, and reading from the second is what delays the output by a sample.
		out[0] = append(out[0], d.resample[0].Resample(mid[1:length+1])...)
		if stereo {
			out[1] = append(out[1], d.resample[1].Resample(side[1:length+1])...)
		}
	}
	return out, nil
}

// skipRedundant reads past the low-bitrate redundancy a packet may carry.
func (d *Decoder) skipRedundant(dec *rangecoder.Decoder, h Header, frames, subframes int, stereo bool) {
	if !h.LBRR[0] && !h.LBRR[1] {
		return
	}
	for i := range frames {
		for c := range d.config.Channels {
			if !h.LBRRFrame[c][i] {
				continue
			}
			if stereo && c == 0 {
				DecodeStereoPrediction(dec)
				if !h.LBRRFrame[1][i] {
					DecodeMidOnly(dec)
				}
			}
			mode := CodeIndependently
			if i > 0 && h.LBRRFrame[c][i-1] {
				mode = CodeConditionally
			}
			// A redundant frame is always of active speech, so it reads from the active distribution
			// whatever the activity flags say.
			ix := DecodeIndices(dec, d.config, &d.ch[c].frames, mode, true, true)
			DecodePulses(dec, ix.SignalType, ix.QuantOffsetType, d.config.FrameLength())
		}
	}
}

// decodeFrame reads one channel's frame and synthesises it at the internal rate.
func (d *Decoder) decodeFrame(dec *rangecoder.Decoder, channel int, mode CodingMode,
	active bool, subframes int,
) []int16 {
	cb := codebookFor(d.config.SampleRateKHz)
	st := &d.ch[channel]

	ix := DecodeIndices(dec, d.config, &st.frames, mode, active, false)
	pulses := DecodePulses(dec, ix.SignalType, ix.QuantOffsetType, d.config.FrameLength())

	var p Params
	p.GainsQ16 = DequantiseGains(ix.GainIndices[:], &st.prevGain, mode == CodeConditionally, subframes)

	nlsf := DecodeNLSF(ix.NLSFIndices[:cb.order+1], cb)
	p.LPCQ12[1] = NLSFToLPC(nlsf, cb.order)
	p.LPCQ12[0] = p.LPCQ12[1]

	if ix.NLSFInterpolation < 4 && st.prevNLSF != nil {
		// The frame's first half runs a filter part way between this frame's spectrum and the last.
		mid := make([]int16, cb.order)
		for k := range cb.order {
			mid[k] = st.prevNLSF[k] + int16(int32(ix.NLSFInterpolation)*int32(nlsf[k]-st.prevNLSF[k])>>2)
		}
		p.LPCQ12[0] = NLSFToLPC(mid, cb.order)
	} else {
		// With nothing to interpolate from, the synthesis must not rebuild its filter mid-frame
		// either, which it decides from this same value.
		ix.NLSFInterpolation = 4
	}
	st.prevNLSF = nlsf

	lags := make([]int, subframes)
	taps := make([]int16, ltpOrder*subframes)
	var scale int16
	if ix.SignalType == TypeVoiced {
		lags = DecodePitchLags(ix.LagIndex, ix.ContourIndex, d.config.SampleRateKHz, subframes)
		taps = DecodeLTPCoefficients(ix.Periodicity, ix.LTPIndices[:], subframes)
		scale = LTPScale(ix.LTPScaleIndex)
	}

	return st.syn.Synthesise(ix, p, lags, taps, scale, pulses, subframes)
}
