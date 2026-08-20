// Package celt implements the MDCT layer of the Opus codec, RFC 6716 section 4.3.
//
// CELT splits the MDCT spectrum into bands that roughly follow the ear's critical bands, and codes
// each band's energy separately from its shape. Coding the energy on its own is what preserves the
// spectral envelope; the shape is a unit-norm vector coded with a pyramid vector quantiser.
package celt

import "fmt"

// NumBands is the count of coded bands in the standard Opus mode.
const NumBands = 21

// BandEdges gives the band boundaries in MDCT bins for a 2.5 ms frame, as cumulative bin counts.
//
// Derived from the per-band bin counts of RFC 6716 table 55: the 2.5 ms column reads
// 1,1,1,1,1,1,1,1,2,2,2,2,4,4,4,6,6,8,12,18,22, and these are its running totals. Longer frames
// scale by the same factor as their length, so one table covers all four.
var BandEdges = [NumBands + 1]int{
	0, 1, 2, 3, 4, 5, 6, 7, 8, 10, 12, 14, 16, 20, 24, 28, 34, 40, 48, 60, 78, 100,
}

// BandFrequencies gives each band's lower edge in hertz, from the same table. The final entry is the
// upper edge of the last band.
var BandFrequencies = [NumBands + 1]int{
	0, 200, 400, 600, 800, 1000, 1200, 1400, 1600, 2000, 2400, 2800, 3200,
	4000, 4800, 5600, 6800, 8000, 9600, 12000, 15600, 20000,
}

// FrameSize identifies one of the four CELT frame lengths by its shift from the shortest.
//
// The reference calls this LM, and it indexes several tables directly, so it is carried as the shift
// rather than as a sample count.
type FrameSize int

// The four frame lengths, at 48 kHz: 2.5, 5, 10 and 20 milliseconds.
const (
	Frame2p5ms FrameSize = iota
	Frame5ms
	Frame10ms
	Frame20ms
)

// Samples returns the frame length at the 48 kHz rate Opus always decodes to.
func (f FrameSize) Samples() int { return 120 << f }

// FrameSizeForSamples maps a sample count onto its frame size.
func FrameSizeForSamples(n int) (FrameSize, error) {
	for f := Frame2p5ms; f <= Frame20ms; f++ {
		if f.Samples() == n {
			return f, nil
		}
	}
	return 0, fmt.Errorf("celt: %d samples is not a CELT frame length", n)
}

// Bins returns the MDCT bins in band b at this frame size.
func (f FrameSize) Bins(b int) int {
	return (BandEdges[b+1] - BandEdges[b]) << f
}

// BandStart returns the first MDCT bin of band b at this frame size.
func (f FrameSize) BandStart(b int) int { return BandEdges[b] << f }

// TotalBins returns the coded bins across every band at this frame size.
//
// This is below the frame length: the bands stop at 20 kHz, and the bins above that are not coded.
func (f FrameSize) TotalBins() int { return BandEdges[NumBands] << f }

// BandForBandwidth returns the number of bands a bandwidth codes.
//
// Narrower bandwidths stop earlier, so the band layout is shared and only the end moves.
func BandForBandwidth(endFrequency int) int {
	for b := NumBands; b > 0; b-- {
		if BandFrequencies[b] <= endFrequency {
			return b
		}
	}
	return 0
}
