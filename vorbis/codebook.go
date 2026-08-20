package vorbis

import (
	"errors"
	"fmt"
	"math"

	"github.com/faide/goxiph/internal/bitio"
)

// ErrBadCodebook reports a codebook that violates Vorbis I section 3.2.1.
var ErrBadCodebook = errors.New("vorbis: invalid codebook")

// codebookSync is the 24-bit pattern opening every packed codebook.
const codebookSync = 0x564342

// maxCodewordLength is the widest codeword the five-bit length field can express.
const maxCodewordLength = 32

// Codebook is a decoded Vorbis codebook: a Huffman code over entry numbers, optionally paired with
// a lookup table turning an entry number into a vector of scalars.
type Codebook struct {
	Dimensions int
	Entries    int
	LookupType int

	MinimumValue  float32
	DeltaValue    float32
	ValueBits     uint
	SequenceP     bool
	LookupValues  int
	Multiplicands []uint32

	lengths []uint8
	codes   []uint32

	// tree holds two int32 per node: a positive value is a child node index, a negative value is
	// -(entry+1) for a leaf, and zero marks a branch no codeword reaches.
	tree []int32

	// single is the entry number of a book with exactly one used codeword, or -1.
	//
	// Errata 20150226: such a book is underpopulated and therefore malformed, but streams using it
	// exist. Reading from one returns that entry and sinks one bit of either value.
	single int32
}

// ilog returns the position of the highest set bit, counting from one. Vorbis I 9.2.1.
func ilog(x int32) uint {
	var n uint
	for x > 0 {
		n++
		x >>= 1
	}
	return n
}

// float32Unpack converts the Vorbis packed float representation. Vorbis I 9.2.2.
//
// This is not IEEE 754: the mantissa is 21 bits, the exponent is biased by 788, and the sign is a
// separate high bit rather than part of a normalised layout.
func float32Unpack(x uint32) float32 {
	mantissa := float64(x & 0x1fffff)
	if x&0x80000000 != 0 {
		mantissa = -mantissa
	}
	exponent := int((x & 0x7fe00000) >> 21)
	return float32(mantissa * math.Pow(2, float64(exponent-788)))
}

// lookup1Values returns the largest n with n^dimensions <= entries. Vorbis I 9.2.3.
//
// Computed by integer search rather than by rounding a logarithm, because the boundary cases are
// exactly where a floating-point result lands on the wrong side.
func lookup1Values(entries, dimensions int) int {
	if dimensions <= 0 || entries < 0 {
		return 0
	}
	n := 0
	for {
		next := n + 1
		p := 1
		overflow := false
		for range dimensions {
			p *= next
			if p > entries {
				overflow = true
				break
			}
		}
		if overflow {
			return n
		}
		n = next
	}
}

// readCodebook decodes one packed codebook. Vorbis I 3.2.1.
func readCodebook(r *bitio.LSBReader) (*Codebook, error) {
	sync, err := r.Read(24)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at sync pattern", ErrBadCodebook)
	}
	if sync != codebookSync {
		return nil, fmt.Errorf("%w: sync %#06x, want %#06x", ErrBadCodebook, sync, codebookSync)
	}

	c := &Codebook{single: -1}
	dims, err := r.Read(16)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at dimensions", ErrBadCodebook)
	}
	entries, err := r.Read(24)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at entries", ErrBadCodebook)
	}
	c.Dimensions = int(dims)
	c.Entries = int(entries)
	if c.Entries == 0 {
		return nil, fmt.Errorf("%w: zero entries", ErrBadCodebook)
	}
	// Each entry costs at least one bit of length data, so a count beyond the packet cannot be
	// honest and must not drive an allocation.
	if c.Entries > r.BitsLeft() {
		return nil, fmt.Errorf("%w: %d entries exceed the %d bits left", ErrBadCodebook, c.Entries, r.BitsLeft())
	}

	if err := c.readLengths(r); err != nil {
		return nil, err
	}
	if err := c.buildTree(); err != nil {
		return nil, err
	}
	if err := c.readLookup(r); err != nil {
		return nil, err
	}
	return c, nil
}

// readLengths fills the codeword length list, in either the ordered or the per-entry form.
func (c *Codebook) readLengths(r *bitio.LSBReader) error {
	ordered, err := r.ReadBool()
	if err != nil {
		return fmt.Errorf("%w: truncated at ordered flag", ErrBadCodebook)
	}
	c.lengths = make([]uint8, c.Entries)

	if !ordered {
		sparse, err := r.ReadBool()
		if err != nil {
			return fmt.Errorf("%w: truncated at sparse flag", ErrBadCodebook)
		}
		for i := range c.lengths {
			if sparse {
				used, err := r.ReadBool()
				if err != nil {
					return fmt.Errorf("%w: truncated at entry %d", ErrBadCodebook, i)
				}
				if !used {
					continue // length stays 0, marking the entry unused
				}
			}
			n, err := r.Read(5)
			if err != nil {
				return fmt.Errorf("%w: truncated at entry %d length", ErrBadCodebook, i)
			}
			c.lengths[i] = uint8(n) + 1
		}
		return nil
	}

	// Ordered form: lengths ascend, so only a run length is stored per length.
	n, err := r.Read(5)
	if err != nil {
		return fmt.Errorf("%w: truncated at initial length", ErrBadCodebook)
	}
	current := int(n) + 1
	for entry := 0; entry < c.Entries; {
		bits := ilog(int32(c.Entries - entry))
		run, err := r.Read(bits)
		if err != nil {
			return fmt.Errorf("%w: truncated at run for length %d", ErrBadCodebook, current)
		}
		if entry+int(run) > c.Entries {
			return fmt.Errorf("%w: run of %d at entry %d overruns %d entries",
				ErrBadCodebook, run, entry, c.Entries)
		}
		if current > maxCodewordLength {
			return fmt.Errorf("%w: codeword length %d exceeds %d", ErrBadCodebook, current, maxCodewordLength)
		}
		for i := entry; i < entry+int(run); i++ {
			c.lengths[i] = uint8(current)
		}
		entry += int(run)
		current++
		// A zero-length run cannot make progress and would spin forever on a crafted stream.
		if run == 0 && current > maxCodewordLength+1 {
			return fmt.Errorf("%w: ordered lengths make no progress", ErrBadCodebook)
		}
	}
	return nil
}

// assignCodewords gives each used entry, in entry order, the lowest unused codeword of its length.
// Vorbis I 3.2.1, "Huffman decision tree representation".
//
// The assignment is in entry order, not sorted by length, so it is not the textbook canonical code:
// with lengths [2,4,4,4,4,2,3,3] the spec's own example gives entry 5 the codeword 10, where a
// canonical assignment would give it 01.
func (c *Codebook) assignCodewords() error {
	c.codes = make([]uint32, c.Entries)

	// marker[n] is the next unused codeword of length n, held right-aligned.
	var marker [maxCodewordLength + 1]uint32
	used := 0

	for i, length := range c.lengths {
		if length == 0 {
			continue // unused entry, absent from the tree
		}
		if int(length) > maxCodewordLength {
			return fmt.Errorf("%w: entry %d has length %d", ErrBadCodebook, i, length)
		}
		used++

		code := marker[length]
		// A codeword that has overflowed its length means the tree has no room left.
		if length < maxCodewordLength && code>>length != 0 {
			return fmt.Errorf("%w: overpopulated at entry %d", ErrBadCodebook, i)
		}
		c.codes[i] = code

		// Advance the marker for this length, carrying into shorter lengths when this codeword
		// completes a sibling pair.
		for j := int(length); j > 0; j-- {
			if marker[j]&1 != 0 {
				if j == 1 {
					marker[1]++
				} else {
					marker[j] = marker[j-1] << 1
				}
				break
			}
			marker[j]++
		}
		// Longer markers that hung below the node just taken move to the new node.
		for j := int(length) + 1; j <= maxCodewordLength; j++ {
			if marker[j]>>1 != code {
				break
			}
			code = marker[j]
			marker[j] = marker[j-1] << 1
		}
	}

	if used == 0 {
		return fmt.Errorf("%w: no used entries", ErrBadCodebook)
	}
	if used == 1 {
		return c.acceptSingleEntry()
	}
	// A complete tree leaves every marker with its low bits clear.
	for i := 1; i <= maxCodewordLength; i++ {
		if marker[i]&(math.MaxUint32>>(32-i)) != 0 {
			return fmt.Errorf("%w: underpopulated tree", ErrBadCodebook)
		}
	}
	return nil
}

// acceptSingleEntry handles the one legal underpopulated book. Errata 20150226.
func (c *Codebook) acceptSingleEntry() error {
	for i, length := range c.lengths {
		if length == 0 {
			continue
		}
		if length != 1 {
			return fmt.Errorf("%w: single-entry book declares length %d, must be 1", ErrBadCodebook, length)
		}
		c.single = int32(i)
		return nil
	}
	return fmt.Errorf("%w: no used entries", ErrBadCodebook)
}

// buildTree turns the codeword list into a decode tree.
func (c *Codebook) buildTree() error {
	if err := c.assignCodewords(); err != nil {
		return err
	}
	if c.single >= 0 {
		return nil
	}

	c.tree = make([]int32, 2, 2*c.Entries)
	for i, length := range c.lengths {
		if length == 0 {
			continue
		}
		code := c.codes[i]
		node := int32(0)
		for b := int(length) - 1; b >= 0; b-- {
			idx := 2*node + int32((code>>b)&1)
			if b == 0 {
				if c.tree[idx] != 0 {
					return fmt.Errorf("%w: codeword collision at entry %d", ErrBadCodebook, i)
				}
				c.tree[idx] = -(int32(i) + 1)
				break
			}
			if c.tree[idx] == 0 {
				c.tree = append(c.tree, 0, 0)
				c.tree[idx] = int32(len(c.tree)/2 - 1)
			} else if c.tree[idx] < 0 {
				return fmt.Errorf("%w: codeword at entry %d extends a leaf", ErrBadCodebook, i)
			}
			node = c.tree[idx]
		}
	}
	return nil
}

// readLookup decodes the VQ lookup table, when the book has one.
func (c *Codebook) readLookup(r *bitio.LSBReader) error {
	lookup, err := r.Read(4)
	if err != nil {
		return fmt.Errorf("%w: truncated at lookup type", ErrBadCodebook)
	}
	c.LookupType = int(lookup)

	switch c.LookupType {
	case 0:
		return nil
	case 1, 2:
	default:
		return fmt.Errorf("%w: lookup type %d is reserved", ErrBadCodebook, c.LookupType)
	}

	minRaw, err := r.Read(32)
	if err != nil {
		return fmt.Errorf("%w: truncated at minimum value", ErrBadCodebook)
	}
	deltaRaw, err := r.Read(32)
	if err != nil {
		return fmt.Errorf("%w: truncated at delta value", ErrBadCodebook)
	}
	vb, err := r.Read(4)
	if err != nil {
		return fmt.Errorf("%w: truncated at value bits", ErrBadCodebook)
	}
	seq, err := r.ReadBool()
	if err != nil {
		return fmt.Errorf("%w: truncated at sequence flag", ErrBadCodebook)
	}

	c.MinimumValue = float32Unpack(minRaw)
	c.DeltaValue = float32Unpack(deltaRaw)
	c.ValueBits = uint(vb) + 1
	c.SequenceP = seq

	if c.LookupType == 1 {
		c.LookupValues = lookup1Values(c.Entries, c.Dimensions)
	} else {
		c.LookupValues = c.Entries * c.Dimensions
	}
	if c.LookupValues < 0 || c.LookupValues > r.BitsLeft() {
		return fmt.Errorf("%w: %d lookup values exceed the %d bits left",
			ErrBadCodebook, c.LookupValues, r.BitsLeft())
	}

	c.Multiplicands = make([]uint32, c.LookupValues)
	for i := range c.Multiplicands {
		v, err := r.Read(c.ValueBits)
		if err != nil {
			return fmt.Errorf("%w: truncated at multiplicand %d", ErrBadCodebook, i)
		}
		c.Multiplicands[i] = v
	}
	return nil
}

// DecodeScalar reads one codeword and returns its entry number.
func (c *Codebook) DecodeScalar(r *bitio.LSBReader) (int, error) {
	if c.single >= 0 {
		// Errata 20150226: sink one bit of either value and return the only entry.
		if _, err := r.Read(1); err != nil {
			return 0, err
		}
		return int(c.single), nil
	}

	node := int32(0)
	for range maxCodewordLength + 1 {
		bit, err := r.Read(1)
		if err != nil {
			return 0, err
		}
		v := c.tree[2*node+int32(bit)]
		switch {
		case v < 0:
			return int(-v - 1), nil
		case v == 0:
			return 0, fmt.Errorf("%w: codeword matches no entry", ErrBadCodebook)
		default:
			node = v
		}
	}
	return 0, fmt.Errorf("%w: codeword longer than %d bits", ErrBadCodebook, maxCodewordLength)
}

// EntryVector writes the VQ vector for an entry into dst, which must hold Dimensions values.
// Vorbis I 3.3, "VQ lookup table vector representation".
func (c *Codebook) EntryVector(entry int, dst []float32) error {
	if c.LookupType == 0 {
		return fmt.Errorf("%w: vector requested from a lookup-type-0 book", ErrBadCodebook)
	}
	if entry < 0 || entry >= c.Entries {
		return fmt.Errorf("%w: entry %d outside 0..%d", ErrBadCodebook, entry, c.Entries-1)
	}
	if len(dst) < c.Dimensions {
		return fmt.Errorf("%w: destination holds %d values, need %d", ErrBadCodebook, len(dst), c.Dimensions)
	}

	var last float32
	if c.LookupType == 1 {
		divisor := 1
		for i := range c.Dimensions {
			offset := (entry / divisor) % c.LookupValues
			dst[i] = float32(c.Multiplicands[offset])*c.DeltaValue + c.MinimumValue + last
			if c.SequenceP {
				last = dst[i]
			}
			divisor *= c.LookupValues
		}
		return nil
	}

	offset := entry * c.Dimensions
	for i := range c.Dimensions {
		dst[i] = float32(c.Multiplicands[offset+i])*c.DeltaValue + c.MinimumValue + last
		if c.SequenceP {
			last = dst[i]
		}
	}
	return nil
}

// DecodeVector reads one codeword and writes its VQ vector into dst.
func (c *Codebook) DecodeVector(r *bitio.LSBReader, dst []float32) error {
	entry, err := c.DecodeScalar(r)
	if err != nil {
		return err
	}
	return c.EntryVector(entry, dst)
}

// CodewordLength returns the codeword length of an entry, or zero when it is unused.
func (c *Codebook) CodewordLength(entry int) int {
	if entry < 0 || entry >= len(c.lengths) {
		return 0
	}
	return int(c.lengths[entry])
}

// Codeword returns the assigned codeword of an entry, read most significant bit first.
func (c *Codebook) Codeword(entry int) uint32 {
	if entry < 0 || entry >= len(c.codes) {
		return 0
	}
	return c.codes[entry]
}
