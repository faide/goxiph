package celt

import "github.com/faide/goxiph/internal/rangecoder"

// Energy decoding follows RFC 6716 section 4.3.2, which codes each band's level in the base-2 log
// domain in three passes: a coarse pass at 6 dB resolution, a fine pass whose width the bit
// allocation decides, and a final pass that spends whatever bits are left over.
//
// The coarse pass predicts in two directions at once, from the same band in the previous frame and
// from the previous band in this one. An "intra" frame drops the time prediction, which is what
// makes a stream decodable from a mid-point.

// MaxFineBits bounds the fine energy refinement of any one band.
const MaxFineBits = 8

// predCoef is the time prediction weight per frame size, and betaCoef the frequency one.
//
// Longer frames predict less well from the previous frame, so the weights fall as the frame grows.
// The values are the reference's, expressed as the fractions of 32768 it stores them as.
var (
	predCoef = [4]float32{29440.0 / 32768, 26112.0 / 32768, 21248.0 / 32768, 16384.0 / 32768}
	betaCoef = [4]float32{30147.0 / 32768, 22282.0 / 32768, 12124.0 / 32768, 6554.0 / 32768}
)

// betaIntra is the frequency prediction weight when time prediction is disabled.
const betaIntra float32 = 4915.0 / 32768

// smallEnergyICDF codes a coarse value when too few bits remain for the Laplace model.
var smallEnergyICDF = []byte{2, 1, 0}

// minCoarseEnergy floors the previous frame's energy before prediction, so a fixed-point decoder
// with limited range stays in the same state as a floating-point one.
const minCoarseEnergy float32 = -9

// energyProbModel holds the Laplace parameters for the coarse energy, indexed by frame size, then by
// whether the frame predicts from the previous one, then by band.
//
// Each band contributes a pair: the probability of a zero prediction error, and the decay rate of
// the tails, both in Q8. Adapted from celt/quant_bands.c of the RFC 6716 reference implementation,
// BSD-3-Clause; the specification names the table and never prints it. See NOTICE.
var energyProbModel = [4][2][42]uint8{
	{ // 120 sample frames
		{ // inter
			72, 127, 65, 129, 66, 128, 65, 128, 64, 128, 62, 128, 64, 128,
			64, 128, 92, 78, 92, 79, 92, 78, 90, 79, 116, 41, 115, 40,
			114, 40, 132, 26, 132, 26, 145, 17, 161, 12, 176, 10, 177, 11,
		},
		{ // intra
			24, 179, 48, 138, 54, 135, 54, 132, 53, 134, 56, 133, 55, 132,
			55, 132, 61, 114, 70, 96, 74, 88, 75, 88, 87, 74, 89, 66,
			91, 67, 100, 59, 108, 50, 120, 40, 122, 37, 97, 43, 78, 50,
		},
	},
	{ // 240 sample frames
		{ // inter
			83, 78, 84, 81, 88, 75, 86, 74, 87, 71, 90, 73, 93, 74,
			93, 74, 109, 40, 114, 36, 117, 34, 117, 34, 143, 17, 145, 18,
			146, 19, 162, 12, 165, 10, 178, 7, 189, 6, 190, 8, 177, 9,
		},
		{ // intra
			23, 178, 54, 115, 63, 102, 66, 98, 69, 99, 74, 89, 71, 91,
			73, 91, 78, 89, 86, 80, 92, 66, 93, 64, 102, 59, 103, 60,
			104, 60, 117, 52, 123, 44, 138, 35, 133, 31, 97, 38, 77, 45,
		},
	},
	{ // 480 sample frames
		{ // inter
			61, 90, 93, 60, 105, 42, 107, 41, 110, 45, 116, 38, 113, 38,
			112, 38, 124, 26, 132, 27, 136, 19, 140, 20, 155, 14, 159, 16,
			158, 18, 170, 13, 177, 10, 187, 8, 192, 6, 175, 9, 159, 10,
		},
		{ // intra
			21, 178, 59, 110, 71, 86, 75, 85, 84, 83, 91, 66, 88, 73,
			87, 72, 92, 75, 98, 72, 105, 58, 107, 54, 115, 52, 114, 55,
			112, 56, 129, 51, 132, 40, 150, 33, 140, 29, 98, 35, 77, 42,
		},
	},
	{ // 960 sample frames
		{ // inter
			42, 121, 96, 66, 108, 43, 111, 40, 117, 44, 123, 32, 120, 36,
			119, 33, 127, 33, 134, 34, 139, 21, 147, 23, 152, 20, 158, 25,
			154, 26, 166, 21, 173, 16, 184, 13, 184, 10, 150, 13, 139, 15,
		},
		{ // intra
			22, 178, 63, 114, 74, 82, 84, 83, 92, 82, 103, 62, 96, 72,
			96, 67, 101, 73, 107, 72, 113, 55, 118, 52, 125, 52, 118, 52,
			117, 55, 135, 49, 137, 39, 157, 32, 145, 29, 97, 33, 77, 40,
		},
	}}

// DecodeCoarseEnergy reads the coarse energy for bands [start, end) into energy.
//
// energy carries the previous frame's values in and this frame's out, one slice per channel, because
// the prediction reads them. tell reports the bits already consumed and budget the frame's total, so
// the decoder can fall back when a frame runs short.
func DecodeCoarseEnergy(d *rangecoder.Decoder, energy [][]float32, start, end int,
	frame FrameSize, intra bool, budget int,
) {
	model := &energyProbModel[frame][boolToInt(intra)]

	var coef, beta float32
	if intra {
		coef, beta = 0, betaIntra
	} else {
		coef, beta = predCoef[frame], betaCoef[frame]
	}

	var prev [2]float32
	for i := start; i < end; i++ {
		for c := range energy {
			qi := decodeCoarseValue(d, model, i, budget)

			// The previous frame's value is floored before it is predicted from.
			e := max(energy[c][i], minCoarseEnergy)
			energy[c][i] = coef*e + prev[c] + float32(qi)
			prev[c] += float32(qi) - beta*float32(qi)
		}
	}
}

// decodeCoarseValue reads one coarse prediction error, degrading as the frame runs out of bits.
//
// RFC 6716 section 4.3.2.1: with fewer than fifteen bits left the Laplace model no longer fits, so
// the decoder falls back first to a three-symbol context and then to a single bit, and finally
// assumes -1. Without this a truncated frame would desynchronise rather than degrade.
func decodeCoarseValue(d *rangecoder.Decoder, model *[42]uint8, band, budget int) int {
	remaining := budget - d.Tell()
	switch {
	case remaining >= 15:
		// The table stops at band 20 and the last pair repeats beyond it.
		pi := 2 * min(band, 20)
		return LaplaceDecode(d, uint32(model[pi])<<7, int32(model[pi+1])<<6)
	case remaining >= 2:
		qi := d.DecodeICDF(smallEnergyICDF, 2)
		return (qi >> 1) ^ -(qi & 1)
	case remaining >= 1:
		return -d.DecodeBitLogp(1)
	default:
		return -1
	}
}

// DecodeFineEnergy reads the fine refinement for bands [start, end).
//
// bits[i] is the width the allocation gave band i. The refinement is an integer f in [0, 2^b), and
// the correction it stands for is (f + 1/2)/2^b - 1/2, which centres the step on zero.
func DecodeFineEnergy(d *rangecoder.Decoder, energy [][]float32, start, end int, bits []int) {
	for i := start; i < end; i++ {
		if bits[i] <= 0 {
			continue
		}
		for c := range energy {
			q := d.DecodeBits(uint(bits[i]))
			offset := (float32(q)+0.5)/float32(int32(1)<<bits[i]) - 0.5
			energy[c][i] += offset
		}
	}
}

// DecodeFinalEnergy spends the bits left after everything else on one more level of refinement.
//
// Bands are visited in two passes by the priority the allocation assigned, so the bands that gain
// most from an extra bit receive one first. It returns the bits still unspent, which are wasted.
func DecodeFinalEnergy(d *rangecoder.Decoder, energy [][]float32, start, end int,
	bits, priority []int, bitsLeft int,
) int {
	channels := len(energy)
	for prio := range 2 {
		for i := start; i < end && bitsLeft >= channels; i++ {
			if bits[i] >= MaxFineBits || priority[i] != prio {
				continue
			}
			for c := range energy {
				q := d.DecodeBits(1)
				offset := (float32(q) - 0.5) / float32(int32(1)<<(bits[i]+1))
				energy[c][i] += offset
				bitsLeft--
			}
		}
	}
	return bitsLeft
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
