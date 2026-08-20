package vorbis

import (
	"errors"
	"fmt"

	"github.com/faide/goxiph/internal/bitio"
)

// ErrBadFloor reports a floor configuration that violates the specification.
var ErrBadFloor = errors.New("vorbis: invalid floor")

// floor1RangeTable maps floor1_multiplier onto the Y value range. Vorbis I 7.2.3.
var floor1RangeTable = [4]int{256, 128, 86, 64}

// maxFloor1Values bounds the X list: 31 partitions of at most 8 dimensions, plus the two implicit
// endpoints.
const maxFloor1Values = 31*8 + 2

// Floor is either floor type 0 or floor type 1.
//
// Floor 0 is legacy and no current encoder emits it; it is parsed so that setup headers using it are
// read correctly, but synthesis is unimplemented.
type Floor struct {
	Type int
	One  *Floor1
}

// Floor1 is a floor of type 1: a piecewise-linear spectral envelope in the log domain.
type Floor1 struct {
	PartitionClassList []int
	ClassDimensions    []int
	ClassSubclasses    []int
	ClassMasterbooks   []int
	SubclassBooks      [][]int
	Multiplier         int
	XList              []int

	// sortedOrder holds XList indices in ascending X order, computed once at setup because curve
	// synthesis needs it on every packet.
	sortedOrder []int
}

// Values is the number of points in the floor's X list.
func (f *Floor1) Values() int { return len(f.XList) }

// readFloor decodes one floor configuration from the setup header.
func readFloor(r *bitio.LSBReader, codebooks []*Codebook) (*Floor, error) {
	t, err := r.Read(16)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at floor type", ErrBadFloor)
	}
	switch t {
	case 0:
		f, err := skipFloor0(r)
		if err != nil {
			return nil, err
		}
		return f, nil
	case 1:
		one, err := readFloor1(r, codebooks)
		if err != nil {
			return nil, err
		}
		return &Floor{Type: 1, One: one}, nil
	default:
		return nil, fmt.Errorf("%w: type %d, want 0 or 1", ErrBadFloor, t)
	}
}

// skipFloor0 consumes a floor 0 configuration. Vorbis I 6.2.1.
func skipFloor0(r *bitio.LSBReader) (*Floor, error) {
	fields := []uint{8, 16, 16, 6, 8} // order, rate, bark_map_size, amplitude_bits, amplitude_offset
	for _, n := range fields {
		if _, err := r.Read(n); err != nil {
			return nil, fmt.Errorf("%w: truncated in floor 0 configuration", ErrBadFloor)
		}
	}
	n, err := r.Read(4)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at floor 0 book count", ErrBadFloor)
	}
	for i := range int(n) + 1 {
		if _, err := r.Read(8); err != nil {
			return nil, fmt.Errorf("%w: truncated at floor 0 book %d", ErrBadFloor, i)
		}
	}
	return &Floor{Type: 0}, nil
}

// readFloor1 decodes a floor 1 configuration. Vorbis I 7.2.2.
func readFloor1(r *bitio.LSBReader, codebooks []*Codebook) (*Floor1, error) {
	f := &Floor1{}

	partitions, err := r.Read(5)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at partition count", ErrBadFloor)
	}
	f.PartitionClassList = make([]int, partitions)
	maxClass := -1
	for i := range f.PartitionClassList {
		v, err := r.Read(4)
		if err != nil {
			return nil, fmt.Errorf("%w: truncated at partition %d class", ErrBadFloor, i)
		}
		f.PartitionClassList[i] = int(v)
		maxClass = max(maxClass, int(v))
	}

	f.ClassDimensions = make([]int, maxClass+1)
	f.ClassSubclasses = make([]int, maxClass+1)
	f.ClassMasterbooks = make([]int, maxClass+1)
	f.SubclassBooks = make([][]int, maxClass+1)
	for i := range maxClass + 1 {
		dim, err := r.Read(3)
		if err != nil {
			return nil, fmt.Errorf("%w: truncated at class %d dimensions", ErrBadFloor, i)
		}
		f.ClassDimensions[i] = int(dim) + 1

		sub, err := r.Read(2)
		if err != nil {
			return nil, fmt.Errorf("%w: truncated at class %d subclasses", ErrBadFloor, i)
		}
		f.ClassSubclasses[i] = int(sub)

		f.ClassMasterbooks[i] = -1
		if sub != 0 {
			mb, err := r.Read(8)
			if err != nil {
				return nil, fmt.Errorf("%w: truncated at class %d masterbook", ErrBadFloor, i)
			}
			if int(mb) >= len(codebooks) {
				return nil, fmt.Errorf("%w: class %d masterbook %d, only %d codebooks", ErrBadFloor, i, mb, len(codebooks))
			}
			f.ClassMasterbooks[i] = int(mb)
		}

		f.SubclassBooks[i] = make([]int, 1<<sub)
		for j := range f.SubclassBooks[i] {
			v, err := r.Read(8)
			if err != nil {
				return nil, fmt.Errorf("%w: truncated at class %d subclass book %d", ErrBadFloor, i, j)
			}
			book := int(v) - 1
			if book >= len(codebooks) {
				return nil, fmt.Errorf("%w: class %d subclass book %d out of range", ErrBadFloor, i, book)
			}
			f.SubclassBooks[i][j] = book
		}
	}

	mult, err := r.Read(2)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at multiplier", ErrBadFloor)
	}
	f.Multiplier = int(mult) + 1

	rangebits, err := r.Read(4)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at rangebits", ErrBadFloor)
	}

	// Points 0 and 1 are implicit and bracket the range.
	f.XList = make([]int, 0, maxFloor1Values)
	f.XList = append(f.XList, 0, 1<<rangebits)
	for i, class := range f.PartitionClassList {
		for range f.ClassDimensions[class] {
			v, err := r.Read(uint(rangebits))
			if err != nil {
				return nil, fmt.Errorf("%w: truncated at partition %d X value", ErrBadFloor, i)
			}
			f.XList = append(f.XList, int(v))
		}
	}
	if len(f.XList) > maxFloor1Values {
		return nil, fmt.Errorf("%w: %d X values exceed %d", ErrBadFloor, len(f.XList), maxFloor1Values)
	}

	f.buildSortOrder()
	return f, nil
}

// buildSortOrder records the X list in ascending order. Vorbis I 7.2.4 step 2 sorts X, Y and the
// step-2 flags together; only the permutation depends on setup, so it is computed once.
func (f *Floor1) buildSortOrder() {
	f.sortedOrder = make([]int, len(f.XList))
	for i := range f.sortedOrder {
		f.sortedOrder[i] = i
	}
	// Insertion sort: the list is short and this keeps equal values in index order.
	for i := 1; i < len(f.sortedOrder); i++ {
		for j := i; j > 0; j-- {
			if f.XList[f.sortedOrder[j-1]] <= f.XList[f.sortedOrder[j]] {
				break
			}
			f.sortedOrder[j-1], f.sortedOrder[j] = f.sortedOrder[j], f.sortedOrder[j-1]
		}
	}
}

// lowNeighbor is the position of the greatest X below X[x], among positions before x. Vorbis I 9.2.4.
func lowNeighbor(v []int, x int) int {
	best, bestVal := 0, -1
	for n := range x {
		if v[n] < v[x] && v[n] > bestVal {
			best, bestVal = n, v[n]
		}
	}
	return best
}

// highNeighbor is the position of the lowest X above X[x], among positions before x. Vorbis I 9.2.5.
func highNeighbor(v []int, x int) int {
	best, bestVal := 0, 1<<62
	for n := range x {
		if v[n] > v[x] && v[n] < bestVal {
			best, bestVal = n, v[n]
		}
	}
	return best
}

// renderPoint solves for Y at X on the line through (x0,y0) and (x1,y1). Vorbis I 9.2.6.
//
// Integer arithmetic throughout: a floating-point equivalent lands on a different value at the
// rounding boundaries and produces a curve that is almost right.
func renderPoint(x0, y0, x1, y1, x int) int {
	dy := y1 - y0
	adx := x1 - x0
	ady := dy
	if ady < 0 {
		ady = -ady
	}
	if adx == 0 {
		return y0
	}
	off := (ady * (x - x0)) / adx
	if dy < 0 {
		return y0 - off
	}
	return y0 + off
}

// renderLine draws the segment from (x0,y0) to (x1,y1) into v. Vorbis I 9.2.7.
//
// Integer division here rounds toward zero for negative values as well as positive, which Go's
// division already does and C89's did not guarantee.
func renderLine(x0, y0, x1, y1 int, v []int) {
	dy := y1 - y0
	adx := x1 - x0
	if adx == 0 {
		return
	}
	ady := dy
	if ady < 0 {
		ady = -ady
	}
	base := dy / adx

	sy := base + 1
	if dy < 0 {
		sy = base - 1
	}
	absBase := base
	if absBase < 0 {
		absBase = -absBase
	}
	ady -= absBase * adx

	y := y0
	err := 0
	if x0 >= 0 && x0 < len(v) {
		v[x0] = y
	}
	for x := x0 + 1; x < x1; x++ {
		err += ady
		if err >= adx {
			err -= adx
			y += sy
		} else {
			y += base
		}
		if x >= 0 && x < len(v) {
			v[x] = y
		}
	}
}

// floor1State is the per-channel scratch a floor 1 decode needs, allocated once per stream.
type floor1State struct {
	y         []int
	finalY    []int
	step2Flag []bool
	curve     []int
}

func newFloor1State(values, n int) *floor1State {
	return &floor1State{
		y:         make([]int, values),
		finalY:    make([]int, values),
		step2Flag: make([]bool, values),
		curve:     make([]int, n),
	}
}

// DecodePacket reads the floor's Y values for one channel. Vorbis I 7.2.3.
//
// The bool reports whether the channel carries energy this frame. End-of-packet during decode is a
// nominal occurrence and returns unused, matching a clear nonzero flag.
func (f *Floor1) DecodePacket(r *bitio.LSBReader, codebooks []*Codebook, st *floor1State) (bool, error) {
	nonzero, err := r.ReadBool()
	if err != nil || !nonzero {
		return false, nil
	}

	rng := floor1RangeTable[f.Multiplier-1]
	bits := ilog(int32(rng - 1))

	v0, err := r.Read(bits)
	if err != nil {
		return false, nil
	}
	v1, err := r.Read(bits)
	if err != nil {
		return false, nil
	}
	st.y[0] = int(v0)
	st.y[1] = int(v1)

	offset := 2
	for i, class := range f.PartitionClassList {
		cdim := f.ClassDimensions[class]
		cbits := f.ClassSubclasses[class]
		csub := (1 << cbits) - 1
		cval := 0

		if cbits > 0 {
			book := f.ClassMasterbooks[class]
			if book < 0 || book >= len(codebooks) {
				return false, fmt.Errorf("%w: partition %d masterbook %d", ErrBadFloor, i, book)
			}
			v, err := codebooks[book].DecodeScalar(r)
			if err != nil {
				return false, nil // nominal end of packet
			}
			cval = v
		}

		for j := range cdim {
			book := f.SubclassBooks[class][cval&csub]
			cval >>= cbits
			if book < 0 {
				st.y[j+offset] = 0
				continue
			}
			if book >= len(codebooks) {
				return false, fmt.Errorf("%w: partition %d subclass book %d", ErrBadFloor, i, book)
			}
			v, err := codebooks[book].DecodeScalar(r)
			if err != nil {
				return false, nil
			}
			st.y[j+offset] = v
		}
		offset += cdim
	}
	return true, nil
}

// Synthesize computes the floor curve into out, which must hold n values. Vorbis I 7.2.4.
func (f *Floor1) Synthesize(st *floor1State, out []float32) {
	n := len(out)
	values := f.Values()
	rng := floor1RangeTable[f.Multiplier-1]

	// Step 1: unwrap the coded differences into absolute Y values.
	st.step2Flag[0] = true
	st.step2Flag[1] = true
	st.finalY[0] = st.y[0]
	st.finalY[1] = st.y[1]

	for i := 2; i < values; i++ {
		lo := lowNeighbor(f.XList, i)
		hi := highNeighbor(f.XList, i)
		predicted := renderPoint(f.XList[lo], st.finalY[lo], f.XList[hi], st.finalY[hi], f.XList[i])

		val := st.y[i]
		highroom := rng - predicted
		lowroom := predicted
		room := lowroom * 2
		if highroom < lowroom {
			room = highroom * 2
		}

		if val != 0 {
			st.step2Flag[lo] = true
			st.step2Flag[hi] = true
			st.step2Flag[i] = true
			switch {
			case val >= room:
				if highroom > lowroom {
					st.finalY[i] = val - lowroom + predicted
				} else {
					st.finalY[i] = predicted - val + highroom - 1
				}
			case val%2 == 1:
				st.finalY[i] = predicted - (val+1)/2
			default:
				st.finalY[i] = predicted + val/2
			}
		} else {
			st.step2Flag[i] = false
			st.finalY[i] = predicted
		}

		// The specification suggests clamping: a valid setup cannot go out of range, but a crafted
		// one can, and the table lookup below would then be out of bounds.
		st.finalY[i] = min(max(st.finalY[i], 0), rng-1)
	}
	st.finalY[0] = min(max(st.finalY[0], 0), rng-1)
	st.finalY[1] = min(max(st.finalY[1], 0), rng-1)

	// Step 2: draw the curve through the points that survived, in ascending X order.
	curve := st.curve[:n]
	clear(curve)

	hx, lx := 0, 0
	first := f.sortedOrder[0]
	ly := st.finalY[first] * f.Multiplier
	hy := ly

	for i := 1; i < values; i++ {
		idx := f.sortedOrder[i]
		if !st.step2Flag[idx] {
			continue
		}
		hy = st.finalY[idx] * f.Multiplier
		hx = f.XList[idx]
		renderLine(lx, ly, hx, hy, curve)
		lx, ly = hx, hy
	}

	if hx < n {
		renderLine(hx, hy, n, hy, curve)
	}

	for i := range out {
		v := curve[i]
		if v < 0 {
			v = 0
		} else if v > 255 {
			v = 255
		}
		out[i] = floor1InverseDBTable[v]
	}
}
