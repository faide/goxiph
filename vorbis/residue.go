package vorbis

import (
	"errors"
	"fmt"

	"github.com/faide/goxiph/internal/bitio"
)

// ErrBadResidue reports a residue configuration that violates the specification.
var ErrBadResidue = errors.New("vorbis: invalid residue")

// maxResiduePasses is the number of encoding stages a cascade bitmap can express. Vorbis I 8.6.1.
const maxResiduePasses = 8

// Residue configures how the fine spectral detail of a frame is coded.
type Residue struct {
	Type            int
	Begin           int
	End             int
	PartitionSize   int
	Classifications int
	Classbook       int
	Cascade         []int
	Books           [][maxResiduePasses]int

	// classifications is scratch reused across packets, indexed by channel then partition.
	classifications [][]int
}

// readResidue decodes one residue configuration. Vorbis I 8.6.1.
func readResidue(r *bitio.LSBReader, codebooks []*Codebook) (*Residue, error) {
	t, err := r.Read(16)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at residue type", ErrBadResidue)
	}
	if t > 2 {
		return nil, fmt.Errorf("%w: type %d, want 0, 1 or 2", ErrBadResidue, t)
	}

	res := &Residue{Type: int(t)}
	begin, err := r.Read(24)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at begin", ErrBadResidue)
	}
	end, err := r.Read(24)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at end", ErrBadResidue)
	}
	partSize, err := r.Read(24)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at partition size", ErrBadResidue)
	}
	classes, err := r.Read(6)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at classification count", ErrBadResidue)
	}
	classbook, err := r.Read(8)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at classbook", ErrBadResidue)
	}

	res.Begin = int(begin)
	res.End = int(end)
	res.PartitionSize = int(partSize) + 1
	res.Classifications = int(classes) + 1
	res.Classbook = int(classbook)

	if res.Begin > res.End {
		return nil, fmt.Errorf("%w: begin %d exceeds end %d", ErrBadResidue, res.Begin, res.End)
	}
	if res.Classbook >= len(codebooks) {
		return nil, fmt.Errorf("%w: classbook %d, only %d codebooks", ErrBadResidue, res.Classbook, len(codebooks))
	}

	// The classbook must be able to express every classification combination it is asked to carry.
	cb := codebooks[res.Classbook]
	total := 1
	for range cb.Dimensions {
		total *= res.Classifications
		if total > cb.Entries {
			return nil, fmt.Errorf("%w: %d classifications over %d dimensions exceed the classbook's %d entries",
				ErrBadResidue, res.Classifications, cb.Dimensions, cb.Entries)
		}
	}

	res.Cascade = make([]int, res.Classifications)
	for i := range res.Cascade {
		low, err := r.Read(3)
		if err != nil {
			return nil, fmt.Errorf("%w: truncated at cascade %d", ErrBadResidue, i)
		}
		flag, err := r.ReadBool()
		if err != nil {
			return nil, fmt.Errorf("%w: truncated at cascade %d flag", ErrBadResidue, i)
		}
		high := uint32(0)
		if flag {
			high, err = r.Read(5)
			if err != nil {
				return nil, fmt.Errorf("%w: truncated at cascade %d high bits", ErrBadResidue, i)
			}
		}
		res.Cascade[i] = int(high)*8 + int(low)
	}

	res.Books = make([][maxResiduePasses]int, res.Classifications)
	for i := range res.Books {
		for j := range maxResiduePasses {
			if res.Cascade[i]&(1<<j) == 0 {
				res.Books[i][j] = -1
				continue
			}
			v, err := r.Read(8)
			if err != nil {
				return nil, fmt.Errorf("%w: truncated at book [%d][%d]", ErrBadResidue, i, j)
			}
			if int(v) >= len(codebooks) {
				return nil, fmt.Errorf("%w: book [%d][%d] is %d, only %d codebooks", ErrBadResidue, i, j, v, len(codebooks))
			}
			// Vorbis I 8.6.1: every residue book must carry a value mapping.
			if codebooks[v].LookupType == 0 {
				return nil, fmt.Errorf("%w: book [%d][%d] has no value mapping", ErrBadResidue, i, j)
			}
			res.Books[i][j] = int(v)
		}
	}
	return res, nil
}

// Decode reads the residue vectors for one frame into out, one slice per channel.
//
// doNotDecode marks channels the mapping has already accounted for; those vectors are still zeroed.
// Vorbis I 8.6.2.
func (res *Residue) Decode(r *bitio.LSBReader, codebooks []*Codebook, out [][]float32, doNotDecode []bool) error {
	ch := len(out)
	if ch == 0 {
		return nil
	}
	for _, v := range out {
		clear(v)
	}

	if res.Type == 2 {
		return res.decodeType2(r, codebooks, out, doNotDecode)
	}
	return res.decodeInto(r, codebooks, out, doNotDecode, len(out[0]))
}

// decodeType2 interleaves every channel into one vector, decodes it as type 1, then deinterleaves.
// Vorbis I 8.6.5.
//
// The interleaving is the difference that matters: types 0 and 1 partition each channel separately,
// so mistaking one for another yields plausible-sounding noise rather than an error.
func (res *Residue) decodeType2(r *bitio.LSBReader, codebooks []*Codebook, out [][]float32, doNotDecode []bool) error {
	ch := len(out)
	n := len(out[0])

	allSkipped := true
	for _, skip := range doNotDecode {
		if !skip {
			allSkipped = false
			break
		}
	}
	if allSkipped {
		return nil // vectors are already zeroed
	}

	// One vector of ch*n, decoded as a single type 1 channel.
	flat := make([][]float32, 1)
	flat[0] = make([]float32, ch*n)
	if err := res.decodeInto(r, codebooks, flat, []bool{false}, ch*n); err != nil {
		return err
	}

	for i := range n {
		for j := range ch {
			out[j][i] = flat[0][i*ch+j]
		}
	}
	return nil
}

// decodeInto runs the shared classification and pass loop. Vorbis I 8.6.2.
func (res *Residue) decodeInto(r *bitio.LSBReader, codebooks []*Codebook, out [][]float32, doNotDecode []bool, actualSize int) error {
	ch := len(out)

	begin := min(res.Begin, actualSize)
	end := min(res.End, actualSize)
	nToRead := end - begin
	if nToRead <= 0 {
		return nil
	}
	partitionsToRead := nToRead / res.PartitionSize
	if partitionsToRead == 0 {
		return nil
	}

	classbook := codebooks[res.Classbook]
	classwordsPerCodeword := classbook.Dimensions
	if classwordsPerCodeword <= 0 {
		return fmt.Errorf("%w: classbook has %d dimensions", ErrBadResidue, classwordsPerCodeword)
	}

	// Scratch grows to fit and is then reused, so steady-state decode does not allocate.
	if len(res.classifications) < ch {
		res.classifications = make([][]int, ch)
	}
	need := partitionsToRead + classwordsPerCodeword
	for j := range ch {
		if len(res.classifications[j]) < need {
			res.classifications[j] = make([]int, need)
		}
	}

	vec := make([]float32, classbook.Dimensions)

	for pass := range maxResiduePasses {
		partitionCount := 0
		for partitionCount < partitionsToRead {
			if pass == 0 {
				for j := range ch {
					if doNotDecode[j] {
						continue
					}
					temp, err := classbook.DecodeScalar(r)
					if err != nil {
						return nil // end of packet is nominal
					}
					// Classifications are packed most significant first, so unpack descending.
					for i := classwordsPerCodeword - 1; i >= 0; i-- {
						res.classifications[j][i+partitionCount] = temp % res.Classifications
						temp /= res.Classifications
					}
				}
			}

			for range classwordsPerCodeword {
				if partitionCount >= partitionsToRead {
					break
				}
				for j := range ch {
					if doNotDecode[j] {
						continue
					}
					vqclass := res.classifications[j][partitionCount]
					vqbook := res.Books[vqclass][pass]
					if vqbook < 0 {
						continue
					}
					offset := begin + partitionCount*res.PartitionSize
					if err := res.decodePartition(r, codebooks[vqbook], out[j], offset, vec); err != nil {
						return nil // end of packet is nominal
					}
				}
				partitionCount++
			}
		}
	}
	return nil
}

// decodePartition accumulates one partition. Vorbis I 8.6.3 and 8.6.4.
//
// Types 0 and 1 differ only in interleave: type 0 strides by the number of sub-vectors, type 1
// writes consecutively.
func (res *Residue) decodePartition(r *bitio.LSBReader, book *Codebook, v []float32, offset int, vec []float32) error {
	n := res.PartitionSize
	dim := book.Dimensions
	if dim <= 0 {
		return fmt.Errorf("%w: book has %d dimensions", ErrBadResidue, dim)
	}
	if len(vec) < dim {
		vec = make([]float32, dim)
	}

	if res.Type == 0 {
		step := n / dim
		if step == 0 {
			return fmt.Errorf("%w: partition size %d is smaller than book dimension %d", ErrBadResidue, n, dim)
		}
		for i := range step {
			if err := book.DecodeVector(r, vec); err != nil {
				return err
			}
			for j := range dim {
				idx := offset + i + j*step
				if idx < len(v) {
					v[idx] += vec[j]
				}
			}
		}
		return nil
	}

	// Types 1 and 2 write the partition in order.
	for i := 0; i < n; {
		if err := book.DecodeVector(r, vec); err != nil {
			return err
		}
		for j := range dim {
			if i >= n {
				break
			}
			idx := offset + i
			if idx < len(v) {
				v[idx] += vec[j]
			}
			i++
		}
	}
	return nil
}
