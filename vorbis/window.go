package vorbis

import "math"

// window holds one precomputed window shape.
//
// A long block has four shapes, chosen by the two window flags in the packet: its left and right
// slopes narrow to short-block width when the neighbour on that side is a short block. Using the
// same symmetric shape everywhere still sounds like audio, so the mistake survives casual listening
// and shows up only against a reference decoder.
type window struct {
	data []float32
}

// windowSet holds every shape a stream can use, built once at decoder setup.
type windowSet struct {
	short window
	// long is indexed by [previousIsLong][nextIsLong].
	long [2][2]window
}

// slope is the Vorbis window function. Vorbis I 4.3.1.
//
// y = sin(pi/2 * sin^2(x)) satisfies sin^2(a) + sin^2(pi/2 - a) = 1, which is the Princen-Bradley
// condition the overlap-add depends on.
func slope(x float64) float32 {
	s := math.Sin(x)
	return float32(math.Sin(math.Pi / 2 * s * s))
}

// newWindow builds one window of size n. blockFlag reports a long block; prevLong and nextLong say
// whether the neighbouring blocks are long, which only matters for a long block.
func newWindow(n, short int, blockFlag, prevLong, nextLong bool) window {
	w := window{data: make([]float32, n)}
	center := n / 2

	leftStart, leftEnd, leftN := 0, center, n/2
	if blockFlag && !prevLong {
		leftStart = n/4 - short/4
		leftEnd = n/4 + short/4
		leftN = short / 2
	}

	rightStart, rightEnd, rightN := center, n, n/2
	if blockFlag && !nextLong {
		rightStart = n*3/4 - short/4
		rightEnd = n*3/4 + short/4
		rightN = short / 2
	}

	// Below leftStart the window is zero and stays zero.
	for i := leftStart; i < leftEnd; i++ {
		x := (float64(i-leftStart) + 0.5) / float64(leftN) * math.Pi / 2
		w.data[i] = slope(x)
	}
	for i := leftEnd; i < rightStart; i++ {
		w.data[i] = 1
	}
	for i := rightStart; i < rightEnd; i++ {
		x := (float64(i-rightStart)+0.5)/float64(rightN)*math.Pi/2 + math.Pi/2
		w.data[i] = slope(x)
	}
	// Above rightEnd the window is zero and stays zero.
	return w
}

// newWindowSet builds every shape for a stream's two block sizes.
func newWindowSet(short, long int) *windowSet {
	ws := &windowSet{short: newWindow(short, short, false, false, false)}
	for prev := range 2 {
		for next := range 2 {
			ws.long[prev][next] = newWindow(long, short, true, prev == 1, next == 1)
		}
	}
	return ws
}

// forBlock returns the window for a block, given its neighbours.
func (ws *windowSet) forBlock(blockFlag, prevLong, nextLong bool) []float32 {
	if !blockFlag {
		return ws.short.data
	}
	p, n := 0, 0
	if prevLong {
		p = 1
	}
	if nextLong {
		n = 1
	}
	return ws.long[p][n].data
}
