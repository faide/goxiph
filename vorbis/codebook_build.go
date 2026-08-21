package vorbis

import (
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/faide/goxiph/internal/bitio"
)

// An encoder has to put its codebooks in the stream, so it needs to build them rather than read
// them: a Huffman code fitted to how often each entry is used, and the packed form a decoder reads
// back. Nothing here is taken from another implementation's tables — the books are fitted to the
// signal at hand, which is both the licence-safe route and the one that adapts.
//
// Vorbis I sections 3.2.1 and 4.2.4.

// NewCodebook builds a scalar codebook from per-entry codeword lengths.
//
// The lengths must form a complete Huffman code, which is what the decoder checks; BuildLengths
// produces such a set. An entry of length zero is unused and gets no codeword.
func NewCodebook(dimensions int, lengths []uint8) (*Codebook, error) {
	if dimensions <= 0 || dimensions > 0xFFFF {
		return nil, fmt.Errorf("%w: %d dimensions", ErrBadCodebook, dimensions)
	}
	if len(lengths) == 0 || len(lengths) > 0xFFFFFF {
		return nil, fmt.Errorf("%w: %d entries", ErrBadCodebook, len(lengths))
	}
	c := &Codebook{
		Dimensions: dimensions,
		Entries:    len(lengths),
		single:     -1,
		lengths:    slices.Clone(lengths),
	}
	if err := c.buildTree(); err != nil {
		return nil, err
	}
	return c, nil
}

// SetLookup gives a codebook the vector table that turns an entry number into scalars.
//
// Only lookup type 1 is produced: the vectors are the points of a regular lattice, so the table
// costs one value per axis rather than one per entry. Type 2 stores every vector outright and is
// larger for nothing an encoder here needs.
func (c *Codebook) SetLookup(minimum, delta float32, valueBits uint, sequenceP bool, multiplicands []uint32) error {
	values := lookup1Values(c.Entries, c.Dimensions)
	if len(multiplicands) != values {
		return fmt.Errorf("%w: %d multiplicands for a lattice of %d", ErrBadCodebook, len(multiplicands), values)
	}
	if valueBits == 0 || valueBits > 32 {
		return fmt.Errorf("%w: %d value bits", ErrBadCodebook, valueBits)
	}
	c.LookupType = 1
	c.MinimumValue = minimum
	c.DeltaValue = delta
	c.ValueBits = valueBits
	c.SequenceP = sequenceP
	c.LookupValues = values
	c.Multiplicands = slices.Clone(multiplicands)
	return nil
}

// BuildLengths fits codeword lengths to how often each entry is used.
//
// Entries never used get length zero and no codeword. Where fewer than two entries are used the
// code cannot be built from frequencies at all, so the shortest legal complete code over the whole
// alphabet is used instead: a book has to stay decodable however lopsided its statistics turn out.
func BuildLengths(counts []int, limit int) ([]uint8, error) {
	if len(counts) == 0 {
		return nil, fmt.Errorf("%w: no entries", ErrBadCodebook)
	}
	if limit < 1 || limit > maxCodewordLength {
		return nil, fmt.Errorf("%w: length limit %d", ErrBadCodebook, limit)
	}

	used := 0
	for _, n := range counts {
		if n > 0 {
			used++
		}
	}
	if used < 2 {
		return flatLengths(len(counts))
	}

	lengths := huffmanLengths(counts, limit)
	// Rounding a fitted code up to the limit can leave it incomplete, and a decoder rejects that.
	// Filling the gap costs a fraction of a bit and keeps the book legal.
	if !complete(lengths) {
		return flatLengths(len(counts))
	}
	return lengths, nil
}

// flatLengths returns the shortest complete code over n entries: a balanced tree, with the leftover
// leaves of a non-power-of-two alphabet one level shallower.
func flatLengths(n int) ([]uint8, error) {
	if n == 1 {
		// A single entry cannot form a complete code. One bit is the shortest a decoder will read.
		return []uint8{1}, nil
	}
	depth := 0
	for 1<<depth < n {
		depth++
	}
	if depth > maxCodewordLength {
		return nil, fmt.Errorf("%w: %d entries need %d bits", ErrBadCodebook, n, depth)
	}
	lengths := make([]uint8, n)
	// The shallow leaves come first, which is what leaves the code complete: 2^depth - n of them
	// stand one level up and absorb the slack.
	short := (1 << depth) - n
	for i := range lengths {
		if i < short {
			lengths[i] = uint8(depth - 1)
		} else {
			lengths[i] = uint8(depth)
		}
	}
	if depth == 0 {
		lengths[0] = 1
	}
	return lengths, nil
}

// huffmanLengths returns length-limited codeword lengths for the used entries.
//
// The fit is by package merge, which gives the optimal code under a depth limit; a plain Huffman
// tree can run deeper than the five-bit length field allows.
func huffmanLengths(counts []int, limit int) []uint8 {
	type item struct {
		entry  int
		weight float64
	}
	var live []item
	for i, n := range counts {
		if n > 0 {
			live = append(live, item{i, float64(n)})
		}
	}
	sort.Slice(live, func(a, b int) bool { return live[a].weight < live[b].weight })

	// A package is a set of entries that will share the next bit. Level by level, packages are
	// paired and merged into the level above, and how many packages an entry ends up inside is its
	// codeword length.
	type pkg struct {
		weight  float64
		members []int
	}
	var packages []pkg
	depth := make([]int, len(counts))

	for range limit {
		merged := make([]pkg, 0, len(live)+len(packages)/2)
		for _, it := range live {
			merged = append(merged, pkg{it.weight, []int{it.entry}})
		}
		for i := 0; i+1 < len(packages); i += 2 {
			m := slices.Concat(packages[i].members, packages[i+1].members)
			merged = append(merged, pkg{packages[i].weight + packages[i+1].weight, m})
		}
		sort.SliceStable(merged, func(a, b int) bool { return merged[a].weight < merged[b].weight })
		packages = merged
	}

	// The first 2(n-1) packages of the final level are the ones the code is built from.
	take := min(2*(len(live)-1), len(packages))
	for _, p := range packages[:take] {
		for _, e := range p.members {
			depth[e]++
		}
	}

	lengths := make([]uint8, len(counts))
	for i, d := range depth {
		if counts[i] > 0 {
			lengths[i] = uint8(min(max(d, 1), limit))
		}
	}
	return lengths
}

// complete reports whether the lengths form a complete Huffman code: every leaf of the tree used,
// none left over.
func complete(lengths []uint8) bool {
	total := 0.0
	for _, l := range lengths {
		if l > 0 {
			total += math.Ldexp(1, -int(l))
		}
	}
	return math.Abs(total-1) < 1e-9
}

// AppendTo writes the codebook in the packed form a setup header carries. Vorbis I 3.2.1.
func (c *Codebook) AppendTo(w *bitio.LSBWriter) error {
	if err := w.Write(codebookSync, 24); err != nil {
		return err
	}
	if err := w.Write(uint32(c.Dimensions), 16); err != nil {
		return err
	}
	if err := w.Write(uint32(c.Entries), 24); err != nil {
		return err
	}

	// The unordered form, one length per entry. The ordered form is shorter only where the lengths
	// happen to be non-decreasing, which a fitted book's are not.
	if err := w.WriteBit(0); err != nil {
		return err
	}
	sparse := slices.Contains(c.lengths, 0)
	if err := w.WriteBit(boolBit(sparse)); err != nil {
		return err
	}
	for _, l := range c.lengths {
		if sparse {
			if err := w.WriteBit(boolBit(l != 0)); err != nil {
				return err
			}
			if l == 0 {
				continue
			}
		}
		if err := w.Write(uint32(l-1), 5); err != nil {
			return err
		}
	}

	if err := w.Write(uint32(c.LookupType), 4); err != nil {
		return err
	}
	if c.LookupType == 0 {
		return nil
	}
	if err := w.Write(float32Pack(c.MinimumValue), 32); err != nil {
		return err
	}
	if err := w.Write(float32Pack(c.DeltaValue), 32); err != nil {
		return err
	}
	if err := w.Write(uint32(c.ValueBits-1), 4); err != nil {
		return err
	}
	if err := w.WriteBit(boolBit(c.SequenceP)); err != nil {
		return err
	}
	for _, m := range c.Multiplicands {
		if err := w.Write(m, c.ValueBits); err != nil {
			return err
		}
	}
	return nil
}

// WriteCodeword emits an entry's codeword.
//
// The bit reader delivers bits in the order they were packed and the decoder descends the tree from
// the codeword's top bit down, so the codeword goes out reversed. Getting this backwards produces a
// stream that decodes to the wrong entries without ever failing, which is the worst kind of wrong.
func (c *Codebook) WriteCodeword(w *bitio.LSBWriter, entry int) error {
	n := c.CodewordLength(entry)
	if n == 0 {
		return fmt.Errorf("%w: entry %d has no codeword", ErrBadCodebook, entry)
	}
	code := c.Codeword(entry)
	var reversed uint32
	for i := range n {
		reversed |= ((code >> uint(i)) & 1) << uint(n-1-i)
	}
	return w.Write(reversed, uint(n))
}

// float32Pack is the inverse of float32Unpack: the Vorbis packed float, which is not IEEE 754.
//
// The mantissa is 21 bits and the exponent biased by 788, so the representable range is wide and the
// precision coarse. A value that will not fit is clamped rather than wrapped, because a wrapped
// exponent turns a small number into an enormous one.
func float32Pack(v float32) uint32 {
	if v == 0 {
		return 0
	}
	sign := uint32(0)
	x := float64(v)
	if x < 0 {
		sign, x = 0x80000000, -x
	}

	// Scale into the mantissa's range, then read off the exponent that took it there.
	exp := 788
	for x >= 0x200000 {
		x /= 2
		exp++
	}
	for x < 0x100000 && exp > 0 {
		x *= 2
		exp--
	}
	mantissa := uint32(math.Round(x))
	if mantissa > 0x1fffff {
		mantissa, exp = 0x1fffff, exp+1
	}
	if exp > 0x3ff {
		mantissa, exp = 0x1fffff, 0x3ff
	}
	return sign | uint32(exp)<<21 | mantissa
}

func boolBit(b bool) uint32 {
	if b {
		return 1
	}
	return 0
}
