package silk

import "github.com/faide/goxiph/internal/rangecoder"

// Every SILK frame begins with a block of indices: which entries of which codebooks it uses, and how
// each is coded relative to what came before. Expanding them into filter coefficients and gains
// comes later; this stage only reads them, in the one order the format allows.
//
// Adapted from silk/decode_indices.c.

// The two signal types a frame can carry. An inactive frame is coded but holds no speech, and a
// voiced one adds a pitch filter that an unvoiced one does not.
const (
	TypeInactive = iota
	TypeUnvoiced
	TypeVoiced
)

// nlsfQuantMaxAmplitude bounds a second-stage residual before it needs an extension symbol.
const nlsfQuantMaxAmplitude = 4

// CodingMode says what a frame may refer back to.
//
// The first frame of a packet stands alone; a later one may code its gain and pitch as changes from
// its predecessor, which is cheaper but leaves it undecodable on its own.
type CodingMode int

// The two coding modes: standing alone, or referring back to the previous frame.
const (
	CodeIndependently CodingMode = iota
	CodeConditionally
)

// nlsfCodebook is one of the two line-spectral codebooks, narrow and wide band.
//
// The two stages work together: the first picks a coarse vector, and the second codes a residual
// whose distribution depends on which vector that was. The select table is what carries that
// dependency, so it has to travel with the codebook rather than beside it.
type nlsfCodebook struct {
	vectors   int
	order     int
	stepSize  int32 // Q16
	invStep   int32 // Q6
	cb1       []uint8
	cb1ICDF   []uint8
	predQ8    []uint8
	selectQ8  []uint8
	ec        []uint8
	deltaMinQ int16Slice
}

// int16Slice names the delta-minimum table's element type, which differs from the rest.
type int16Slice = []int16

var (
	nlsfNarrow = nlsfCodebook{
		vectors: 32, order: 10,
		stepSize: 11796, invStep: 356,
		cb1: nlsfCB1NBMB[:], cb1ICDF: nlsfCB1ICDFNBMB[:], predQ8: nlsfPredNBMB[:],
		selectQ8: nlsfCB2SelectNBMB[:], ec: nlsfCB2ICDFNBMB[:], deltaMinQ: nlsfDeltaMinNBMB[:],
	}
	nlsfWide = nlsfCodebook{
		vectors: 32, order: 16,
		stepSize: 9830, invStep: 427,
		cb1: nlsfCB1WB[:], cb1ICDF: nlsfCB1ICDFWB[:], predQ8: nlsfPredWB[:],
		selectQ8: nlsfCB2SelectWB[:], ec: nlsfCB2ICDFWB[:], deltaMinQ: nlsfDeltaMinWB[:],
	}
)

// codebookFor returns the line-spectral codebook a rate uses. Narrow and medium band share one.
func codebookFor(rateKHz int) *nlsfCodebook {
	if rateKHz == WideBand {
		return &nlsfWide
	}
	return &nlsfNarrow
}

// Indices is one frame's worth of codebook selections.
type Indices struct {
	SignalType      int
	QuantOffsetType int

	// GainIndices is one per subframe. The first is absolute or a change, by coding mode; the rest
	// are always changes from the subframe before.
	GainIndices [MaxSubframesInFrame]int
	// NLSFIndices holds the first-stage vector followed by one residual per coefficient.
	NLSFIndices [17]int
	// NLSFInterpolation is quarters of the way from the previous frame's spectrum to this one's.
	NLSFInterpolation int

	// The pitch fields are read only for a voiced frame.
	LagIndex      int
	ContourIndex  int
	Periodicity   int
	LTPIndices    [MaxSubframesInFrame]int
	LTPScaleIndex int

	// Seed drives the sign randomisation the excitation applies.
	Seed int
}

// frameState is what one channel carries between frames of a packet.
type frameState struct {
	prevSignalType int
	prevLagIndex   int
}

// unpackNLSF returns, for a first-stage vector, which distribution codes each residual and which
// predictor weights apply to it.
//
// Both are packed two coefficients to a byte, because the choice is between few enough options that
// a whole byte each would be waste at these rates.
func (cb *nlsfCodebook) unpackNLSF(index int) (ecIndex []int, predQ8 []uint8) {
	ecIndex = make([]int, cb.order)
	predQ8 = make([]uint8, cb.order)

	sel := cb.selectQ8[index*cb.order/2:]
	for i := 0; i < cb.order; i += 2 {
		entry := sel[i/2]
		ecIndex[i] = int(entry>>1&7) * (2*nlsfQuantMaxAmplitude + 1)
		predQ8[i] = cb.predQ8[i+int(entry&1)*(cb.order-1)]
		ecIndex[i+1] = int(entry>>5&7) * (2*nlsfQuantMaxAmplitude + 1)
		predQ8[i+1] = cb.predQ8[i+int(entry>>4&1)*(cb.order-1)+1]
	}
	return ecIndex, predQ8
}

// pitchLagLowBitsICDF returns the distribution for the fine part of an absolute pitch lag, which is
// uniform over as many steps as the rate resolves.
func pitchLagLowBitsICDF(rateKHz int) []byte {
	switch rateKHz {
	case WideBand:
		return uniform8ICDF[:]
	case MediumBand:
		return uniform6ICDF[:]
	default:
		return uniform4ICDF[:]
	}
}

// pitchContourICDF returns the distribution for the per-subframe deviation from the frame's lag.
func pitchContourICDF(rateKHz, subframes int) []byte {
	if rateKHz == NarrowBand {
		if subframes == SubframesPerFrame {
			return pitchContourNBICDF[:]
		}
		return pitchContour10msNBICDF[:]
	}
	if subframes == SubframesPerFrame {
		return pitchContourICDFTable[:]
	}
	return pitchContour10msICDF[:]
}

// ltpGainICDF returns the gain distribution for one of the three long-term codebooks.
func ltpGainICDF(periodicity int) []byte {
	switch periodicity {
	case 0:
		return ltpGainICDF0[:]
	case 1:
		return ltpGainICDF1[:]
	default:
		return ltpGainICDF2[:]
	}
}

// DecodeIndices reads one frame's codebook selections.
//
// lbrr says the frame is a redundant copy rather than one that will be played. It changes only which
// distribution the signal type comes from, because a redundant frame is always of an active one.
func DecodeIndices(d *rangecoder.Decoder, c Config, st *frameState,
	mode CodingMode, active, lbrr bool,
) Indices {
	var ix Indices
	subframes := c.Subframes()
	cb := codebookFor(c.SampleRateKHz)

	// Signal type and quantiser offset arrive as one symbol. An inactive frame cannot be voiced, so
	// its distribution covers only the two lower types and the offset within them.
	var combined int
	if lbrr || active {
		combined = d.DecodeICDF(typeOffsetVADICDF[:], 8) + 2
	} else {
		combined = d.DecodeICDF(typeOffsetNoVADICDF[:], 8)
	}
	ix.SignalType = combined >> 1
	ix.QuantOffsetType = combined & 1

	// The first gain is absolute unless the frame may refer back, in which case it too is a change.
	// Absolute coding is split so the coarse part can use a distribution shaped by signal type.
	if mode == CodeConditionally {
		ix.GainIndices[0] = d.DecodeICDF(deltaGainICDF[:], 8)
	} else {
		ix.GainIndices[0] = d.DecodeICDF(gainICDF[ix.SignalType][:], 8) << 3
		ix.GainIndices[0] += d.DecodeICDF(uniform8ICDF[:], 8)
	}
	for i := 1; i < subframes; i++ {
		ix.GainIndices[i] = d.DecodeICDF(deltaGainICDF[:], 8)
	}

	// Line spectral frequencies: a coarse vector, then one residual per coefficient from a
	// distribution the coarse vector selects.
	ix.NLSFIndices[0] = d.DecodeICDF(cb.cb1ICDF[(ix.SignalType>>1)*cb.vectors:], 8)
	ecIndex, _ := cb.unpackNLSF(ix.NLSFIndices[0])
	for i := range cb.order {
		v := d.DecodeICDF(cb.ec[ecIndex[i]:], 8)
		// A residual at either end of its range may continue into an extension symbol.
		switch v {
		case 0:
			v -= d.DecodeICDF(nlsfExtensionICDF[:], 8)
		case 2 * nlsfQuantMaxAmplitude:
			v += d.DecodeICDF(nlsfExtensionICDF[:], 8)
		}
		ix.NLSFIndices[i+1] = v - nlsfQuantMaxAmplitude
	}

	// A two-subframe frame is too short to interpolate across, and takes the whole of its own
	// spectrum.
	if subframes == SubframesPerFrame {
		ix.NLSFInterpolation = d.DecodeICDF(nlsfInterpolationICDF[:], 8)
	} else {
		ix.NLSFInterpolation = 4
	}

	if ix.SignalType == TypeVoiced {
		absolute := true
		if mode == CodeConditionally && st.prevSignalType == TypeVoiced {
			// A pitch that has not moved far is coded as a change, which is much cheaper. The symbol
			// reserves zero to mean that it moved too far, in which case the lag follows in full.
			delta := d.DecodeICDF(pitchDeltaICDF[:], 8)
			if delta > 0 {
				ix.LagIndex = st.prevLagIndex + delta - 9
				absolute = false
			}
		}
		if absolute {
			ix.LagIndex = d.DecodeICDF(pitchLagICDF[:], 8) * (c.SampleRateKHz >> 1)
			ix.LagIndex += d.DecodeICDF(pitchLagLowBitsICDF(c.SampleRateKHz), 8)
		}
		st.prevLagIndex = ix.LagIndex

		ix.ContourIndex = d.DecodeICDF(pitchContourICDF(c.SampleRateKHz, subframes), 8)

		// The periodicity chooses among three gain codebooks, from weakest to strongest.
		ix.Periodicity = d.DecodeICDF(ltpPeriodicityICDF[:], 8)
		for i := range subframes {
			ix.LTPIndices[i] = d.DecodeICDF(ltpGainICDF(ix.Periodicity), 8)
		}

		// How much of the previous frame's excitation carries in is only coded where there is no
		// previous frame to inherit the answer from.
		if mode == CodeIndependently {
			ix.LTPScaleIndex = d.DecodeICDF(ltpScaleICDF[:], 8)
		}
	}
	st.prevSignalType = ix.SignalType

	ix.Seed = d.DecodeICDF(uniform4ICDF[:], 8)
	return ix
}
