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
	// collapsing says the stream has just dropped to one coded channel. The packet that does so
	// still delivers two, the second of them the first run through the side channel's resampler, so
	// that what the side was playing decays rather than stopping.
	collapsing bool
	resample   [2]*Resampler
}

// channelState is everything one coded channel carries between frames.
type channelState struct {
	frames   frameState
	prevGain int32
	prevNLSF []int16
	syn      *SynthesisState

	plc plcState
	cng cngState

	// lossCount is how many frames in a row have been concealed, which sets how hard concealment
	// attenuates. prevSignalType and lagPrev are the last good frame's, which both concealment and
	// the frame after it extrapolate from.
	lossCount            int
	prevSignalType       int
	lagPrev              int
	firstFrameAfterReset bool
}

// lagPrevAfterReset is the pitch a channel starts from with no frame behind it, from
// silk/decoder_set_fs.c.
const lagPrevAfterReset = 100

// newChannelState returns a channel with nothing behind it.
func newChannelState(rateKHz, subframes int) channelState {
	return channelState{
		syn:                  NewSynthesisState(rateKHz, subframes),
		lagPrev:              lagPrevAfterReset,
		prevSignalType:       TypeInactive,
		firstFrameAfterReset: true,
	}
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
	d.collapsing = false

	for c := range d.config.Channels {
		// The entropy state in d.ch[c].frames survives; everything else is rebuilt.
		syn, frames := NewSynthesisState(rateKHz, d.config.Subframes()), d.ch[c].frames
		d.ch[c] = newChannelState(rateKHz, d.config.Subframes())
		d.ch[c].syn, d.ch[c].frames = syn, frames
		// From silk/decoder_set_fs.c: the gain index restarts at ten, not at zero.
		d.ch[c].prevGain = gainIndexAfterRateChange
		d.resample[c] = NewResampler(rateKHz)
	}
}

// SetChannels moves the decoder between one coded channel and two.
//
// It is not a fresh start either way. Gaining a channel gives the new one nothing to predict from
// and clears the stereo prediction, but the channel that was already running keeps everything;
// losing one keeps both, because the stream may gain it back.
func (d *Decoder) SetChannels(channels int) {
	if channels == d.config.Channels {
		return
	}
	was := d.config.Channels
	d.config.Channels = channels

	if channels == 2 && was == 1 {
		d.ch[1] = newChannelState(d.config.SampleRateKHz, d.config.Subframes())
		d.ch[1].prevGain = gainIndexAfterRateChange
		d.stereo.DropSide()
		// The new channel's resampler continues the old one's phase rather than starting cold.
		d.resample[1] = d.resample[0].clone()
		return
	}
	d.collapsing = true
}

// gainIndexAfterRateChange is where the gain index restarts when the internal rate changes.
const gainIndexAfterRateChange = 10

// reset clears everything that carries between frames.
func (d *Decoder) reset() {
	for c := range d.config.Channels {
		d.ch[c] = newChannelState(d.config.SampleRateKHz, d.config.Subframes())
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
	d.ch[1] = newChannelState(d.config.SampleRateKHz, d.config.Subframes())
	d.ch[1].frames.prevLagIndex = lagPrevAfterReset
	d.ch[1].prevGain = gainIndexAfterRateChange
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

	// A packet that drops to one coded channel still delivers two, so that the side channel can be
	// let go of rather than cut. Only its first frame does; after that the two are the same.
	collapsing := d.collapsing
	d.collapsing = false

	out := make([][]int16, d.config.Channels)
	if collapsing {
		out = make([][]int16, 2)
	}
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
		copy(mid[2:], d.decodeFrame(dec, 0, mode, header.VAD[0][i], false, subframes))
		if stereo && !midOnly {
			copy(side[2:], d.decodeFrame(dec, 1, mode, header.VAD[1][i], false, subframes))
		}

		if stereo {
			d.stereo.MidSideToLeftRight(mid, side, pred, d.config.SampleRateKHz, length)
			d.prevMidOnly = midOnly
		} else {
			d.stereo.BufferMono(mid, length)
		}

		// One sample in, not at the frame's start: the buffer's first entries are the previous
		// frame's tail, and reading from the second is what delays the output by a sample.
		played := d.resample[0].Resample(mid[1 : length+1])
		out[0] = append(out[0], played...)
		switch {
		case stereo:
			out[1] = append(out[1], d.resample[1].Resample(side[1:length+1])...)
		case collapsing && i == 0:
			// The same samples through the side channel's resampler, whose state still holds what
			// the side was playing. That is what lets it decay instead of stopping.
			out[1] = append(out[1], d.resample[1].Resample(mid[1:length+1])...)
		case collapsing:
			out[1] = append(out[1], played...)
		}
	}
	return out, nil
}

// DecodeFEC reads the redundant copies of earlier frames a packet carries, in place of its own.
//
// An encoder told to expect loss puts a low-rate copy of each frame into the packet after it. Where
// a packet does not arrive, the next one can stand in: what plays is then the lost audio at a lower
// rate rather than an extrapolation of it. A frame with no copy is concealed as before.
func (d *Decoder) DecodeFEC(dec *rangecoder.Decoder, frameMS int) ([][]int16, error) {
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
	length := d.config.FrameLength()

	out := make([][]int16, d.config.Channels)
	for i := range frames {
		// The prediction is coded only where the mid channel has a copy; otherwise the last one
		// stands, as it does for a packet that never arrived at all.
		pred := d.stereo.PrevPrediction()
		midOnly := false
		if stereo && header.LBRRFrame[0][i] {
			pred = DecodeStereoPrediction(dec)
			if !header.LBRRFrame[1][i] {
				midOnly = DecodeMidOnly(dec)
			}
		}
		if stereo && !midOnly && d.prevMidOnly {
			d.resetSide()
		}
		hasSide := !d.prevMidOnly || (stereo && header.LBRRFrame[1][i])

		mid := make([]int16, length+2)
		side := make([]int16, length+2)
		copy(mid[2:], d.fecFrame(dec, 0, i, header, subframes, length))
		if stereo && hasSide {
			copy(side[2:], d.fecFrame(dec, 1, i, header, subframes, length))
		}

		if stereo {
			d.stereo.MidSideToLeftRight(mid, side, pred, d.config.SampleRateKHz, length)
			d.prevMidOnly = midOnly
		} else {
			d.stereo.BufferMono(mid, length)
		}
		out[0] = append(out[0], d.resample[0].Resample(mid[1:length+1])...)
		if stereo {
			out[1] = append(out[1], d.resample[1].Resample(side[1:length+1])...)
		}
	}
	return out, nil
}

// fecFrame reads one channel's redundant copy, or conceals where the packet carries none.
func (d *Decoder) fecFrame(dec *rangecoder.Decoder, channel, i int, h Header,
	subframes, length int,
) []int16 {
	if !h.LBRRFrame[channel][i] {
		return d.concealFrame(channel, length)
	}
	// A redundant frame refers back only to the frame before it, and only where that one had a copy
	// of its own.
	mode := CodeIndependently
	if i > 0 && h.LBRRFrame[channel][i-1] {
		mode = CodeConditionally
	}
	// A redundant copy is always of active speech, so it reads from the active distribution whatever
	// the activity flags say.
	return d.decodeFrame(dec, channel, mode, true, true, subframes)
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
	active, lbrr bool, subframes int,
) []int16 {
	cb := codebookFor(d.config.SampleRateKHz)
	st := &d.ch[channel]

	ix := DecodeIndices(dec, d.config, &st.frames, mode, active, lbrr)
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

	// The first frame after a loss widens its filter's resonances. Concealment left the long-term
	// state describing a signal this filter was not fitted to, and a sharp filter driven by it rings.
	// Without interpolation the two halves are the same slice, so expanding the second covers both.
	if st.lossCount > 0 {
		bwExpand16(p.LPCQ12[1], bweAfterLossQ16)
		if ix.NLSFInterpolation < 4 {
			bwExpand16(p.LPCQ12[0], bweAfterLossQ16)
		}
	}

	lags := make([]int, subframes)
	taps := make([]int16, ltpOrder*subframes)
	var scale int16
	if ix.SignalType == TypeVoiced {
		lags = DecodePitchLags(ix.LagIndex, ix.ContourIndex, d.config.SampleRateKHz, subframes)
		taps = DecodeLTPCoefficients(ix.Periodicity, ix.LTPIndices[:], subframes)
		scale = LTPScale(ix.LTPScaleIndex)
	}

	out := st.syn.Synthesise(ix, p, lags, taps, scale, pulses, subframes,
		st.lossCount, st.prevSignalType, st.lagPrev)

	st.plc.sync(st.syn, len(out))
	st.plc.update(ix, p, lags, taps, scale, subframes, st.syn.subframeLen, cb.order,
		d.config.SampleRateKHz)
	st.lossCount = 0
	st.prevSignalType = ix.SignalType
	st.firstFrameAfterReset = false
	st.plc.glueFrames(out, 0)

	st.cng.sync(st.syn, cb.order)
	if ix.SignalType == TypeInactive {
		st.cng.update(nlsf, p, st.syn.exc[:], subframes, st.syn.subframeLen, cb.order)
	}
	st.cng.silence(cb.order)
	st.lagPrev = lags[subframes-1]
	return out
}

// Conceal produces one packet's worth of samples without a packet, at 48 kHz.
//
// The duration is the one the caller expected, and a packet longer than 20 ms is concealed one frame
// at a time: concealment fades as the loss runs on, and a 60 ms hole should fade three frames' worth
// rather than one.
func (d *Decoder) Conceal(frameMS int) ([][]int16, error) {
	d.config.FrameMS = frameMS
	if err := d.config.Validate(); err != nil {
		return nil, err
	}

	stereo := d.config.Channels == 2
	length := d.config.FrameLength()

	out := make([][]int16, d.config.Channels)
	for range d.config.FramesPerPacket() {
		// With no packet there is nothing to read the prediction from, so the last one stands.
		pred := d.stereo.PrevPrediction()
		if stereo && d.prevMidOnly {
			d.resetSide()
		}

		mid := make([]int16, length+2)
		side := make([]int16, length+2)
		copy(mid[2:], d.concealFrame(0, length))
		// A packet that coded no side channel leaves nothing to extrapolate from, so the side stays
		// silent until one arrives.
		if stereo && !d.prevMidOnly {
			copy(side[2:], d.concealFrame(1, length))
		}

		if stereo {
			d.stereo.MidSideToLeftRight(mid, side, pred, d.config.SampleRateKHz, length)
		} else {
			d.stereo.BufferMono(mid, length)
		}
		out[0] = append(out[0], d.resample[0].Resample(mid[1:length+1])...)
		if stereo {
			out[1] = append(out[1], d.resample[1].Resample(side[1:length+1])...)
		}
	}

	// The gain is unclamped after a loss, so that a signal that was fading is not pulled back up by
	// the limit on how far one frame's gain may move from the last.
	for c := range d.config.Channels {
		d.ch[c].prevGain = gainIndexAfterRateChange
	}
	return out, nil
}

// concealFrame extrapolates one channel's frame and folds it into that channel's history.
func (d *Decoder) concealFrame(channel, length int) []int16 {
	st := &d.ch[channel]
	cb := codebookFor(d.config.SampleRateKHz)

	out := make([]int16, length)
	st.plc.sync(st.syn, length)
	st.lagPrev = st.plc.conceal(st.syn, out, st.lossCount, st.prevSignalType, st.firstFrameAfterReset)
	st.lossCount++

	st.syn.pushOutput(out)
	st.plc.glueFrames(out, st.lossCount)

	st.cng.sync(st.syn, cb.order)
	st.cng.generate(out, cb.order)
	return out
}
