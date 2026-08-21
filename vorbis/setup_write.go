package vorbis

import (
	"fmt"

	"github.com/faide/goxiph/internal/bitio"
)

// The setup header is the encoder's description of itself: the codebooks, the shape of the floor and
// residue coders, how channels map onto them, and the modes a packet may choose between. A decoder
// can only read a stream it has read this from, so writing it correctly is the whole of the contract
// between the two halves.
//
// Vorbis I section 4.2.4.

// AppendTo writes the setup header, framing bit and all.
//
// The header's own packet type and signature come first, as for the other two; the framing bit at
// the end is what a decoder checks to know it read the header to the same boundary the encoder
// wrote it to.
func (s *Setup) AppendTo(dst []byte) ([]byte, error) {
	w := bitio.NewLSBWriter()

	if err := writeCounted(w, len(s.Codebooks), 8, "codebooks"); err != nil {
		return nil, err
	}
	for _, c := range s.Codebooks {
		if err := c.AppendTo(w); err != nil {
			return nil, err
		}
	}

	// The time-domain transforms are a placeholder in every version of the format: one entry, and
	// its value must be zero.
	if err := w.Write(0, 6); err != nil {
		return nil, err
	}
	if err := w.Write(0, 16); err != nil {
		return nil, err
	}

	if err := writeCounted(w, len(s.Floors), 6, "floors"); err != nil {
		return nil, err
	}
	for _, f := range s.Floors {
		if err := f.appendTo(w); err != nil {
			return nil, err
		}
	}

	if err := writeCounted(w, len(s.Residues), 6, "residues"); err != nil {
		return nil, err
	}
	for _, r := range s.Residues {
		if err := r.appendTo(w); err != nil {
			return nil, err
		}
	}

	if err := writeCounted(w, len(s.Mappings), 6, "mappings"); err != nil {
		return nil, err
	}
	for _, m := range s.Mappings {
		if err := m.appendTo(w); err != nil {
			return nil, err
		}
	}

	if err := writeCounted(w, len(s.Modes), 6, "modes"); err != nil {
		return nil, err
	}
	for _, m := range s.Modes {
		if err := w.WriteBit(boolBit(m.BlockFlag)); err != nil {
			return nil, err
		}
		// The window and transform selectors are reserved and must be zero.
		if err := w.Write(0, 16); err != nil {
			return nil, err
		}
		if err := w.Write(0, 16); err != nil {
			return nil, err
		}
		if err := w.Write(uint32(m.Mapping), 8); err != nil {
			return nil, err
		}
	}

	if err := w.WriteBit(1); err != nil {
		return nil, err
	}
	w.AlignByte()

	dst = append(dst, packetSetup)
	dst = append(dst, headerSignature...)
	return append(dst, w.Bytes()...), nil
}

// writeCounted writes a list length, which the format stores one less than it is.
func writeCounted(w *bitio.LSBWriter, n int, bits uint, what string) error {
	if n < 1 || n > 1<<bits {
		return fmt.Errorf("%w: %d %s", ErrBadSetup, n, what)
	}
	return w.Write(uint32(n-1), bits)
}

// appendTo writes one floor configuration. Only type 1 is written; type 0 is legacy and this
// package does not synthesise it, so emitting it would produce a stream it could not read back.
func (f *Floor) appendTo(w *bitio.LSBWriter) error {
	if f.Type != 1 || f.One == nil {
		return fmt.Errorf("%w: floor type %d cannot be written", ErrBadFloor, f.Type)
	}
	if err := w.Write(1, 16); err != nil {
		return err
	}
	one := f.One

	if len(one.PartitionClassList) > 31 {
		return fmt.Errorf("%w: %d partitions", ErrBadFloor, len(one.PartitionClassList))
	}
	if err := w.Write(uint32(len(one.PartitionClassList)), 5); err != nil {
		return err
	}
	for _, c := range one.PartitionClassList {
		if err := w.Write(uint32(c), 4); err != nil {
			return err
		}
	}
	for i := range one.ClassDimensions {
		if err := w.Write(uint32(one.ClassDimensions[i]-1), 3); err != nil {
			return err
		}
		if err := w.Write(uint32(one.ClassSubclasses[i]), 2); err != nil {
			return err
		}
		if one.ClassSubclasses[i] > 0 {
			if err := w.Write(uint32(one.ClassMasterbooks[i]), 8); err != nil {
				return err
			}
		}
		// The book list carries one more than the book number, so that zero can mean "no book".
		for _, b := range one.SubclassBooks[i] {
			if err := w.Write(uint32(b+1), 8); err != nil {
				return err
			}
		}
	}

	if one.Multiplier < 1 || one.Multiplier > 4 {
		return fmt.Errorf("%w: multiplier %d", ErrBadFloor, one.Multiplier)
	}
	if err := w.Write(uint32(one.Multiplier-1), 2); err != nil {
		return err
	}

	// The first two X values are the implicit endpoints and are not written; the width of the rest
	// is chosen from the larger of them.
	if len(one.XList) < 2 {
		return fmt.Errorf("%w: %d floor values", ErrBadFloor, len(one.XList))
	}
	rangeBits := ilog(int32(one.XList[1] - 1))
	if err := w.Write(uint32(rangeBits), 4); err != nil {
		return err
	}
	for _, x := range one.XList[2:] {
		if x >= 1<<rangeBits {
			return fmt.Errorf("%w: X value %d needs more than %d bits", ErrBadFloor, x, rangeBits)
		}
		if err := w.Write(uint32(x), rangeBits); err != nil {
			return err
		}
	}
	return nil
}

// appendTo writes one residue configuration. Vorbis I 8.6.1.
func (r *Residue) appendTo(w *bitio.LSBWriter) error {
	if r.Type < 0 || r.Type > 2 {
		return fmt.Errorf("%w: type %d", ErrBadResidue, r.Type)
	}
	for _, v := range []struct {
		value int
		bits  uint
	}{
		{r.Type, 16}, {r.Begin, 24}, {r.End, 24}, {r.PartitionSize - 1, 24},
		{r.Classifications - 1, 6}, {r.Classbook, 8},
	} {
		if err := w.Write(uint32(v.value), v.bits); err != nil {
			return err
		}
	}

	// The cascade bitmap is split: the low three bits inline, the rest behind a flag, because most
	// residues use one pass and pay five bits for the whole field.
	for _, c := range r.Cascade {
		if err := w.Write(uint32(c&7), 3); err != nil {
			return err
		}
		high := c >> 3
		if err := w.WriteBit(boolBit(high != 0)); err != nil {
			return err
		}
		if high != 0 {
			if err := w.Write(uint32(high), 5); err != nil {
				return err
			}
		}
	}
	for i, c := range r.Cascade {
		for pass := range maxResiduePasses {
			if c&(1<<uint(pass)) == 0 {
				continue
			}
			if err := w.Write(uint32(r.Books[i][pass]), 8); err != nil {
				return err
			}
		}
	}
	return nil
}

// appendTo writes one channel mapping. Vorbis I 4.2.4.
func (m *Mapping) appendTo(w *bitio.LSBWriter) error {
	if err := w.Write(0, 16); err != nil {
		return err
	}

	// A single submap is the common case and is written as a flag rather than a count.
	submaps := len(m.SubmapFloor)
	if err := w.WriteBit(boolBit(submaps > 1)); err != nil {
		return err
	}
	if submaps > 1 {
		if err := w.Write(uint32(submaps-1), 4); err != nil {
			return err
		}
	}

	if err := w.WriteBit(boolBit(len(m.CouplingSteps) > 0)); err != nil {
		return err
	}
	if len(m.CouplingSteps) > 0 {
		if err := w.Write(uint32(len(m.CouplingSteps)-1), 8); err != nil {
			return err
		}
		bits := ilog(int32(len(m.Mux) - 1))
		for _, s := range m.CouplingSteps {
			if err := w.Write(uint32(s.Magnitude), bits); err != nil {
				return err
			}
			if err := w.Write(uint32(s.Angle), bits); err != nil {
				return err
			}
		}
	}

	// Two reserved bits, which must be zero.
	if err := w.Write(0, 2); err != nil {
		return err
	}
	if submaps > 1 {
		for _, mux := range m.Mux {
			if err := w.Write(uint32(mux), 4); err != nil {
				return err
			}
		}
	}
	for i := range submaps {
		// Eight bits of unused time configuration precede each submap.
		if err := w.Write(0, 8); err != nil {
			return err
		}
		if err := w.Write(uint32(m.SubmapFloor[i]), 8); err != nil {
			return err
		}
		if err := w.Write(uint32(m.SubmapResidue[i]), 8); err != nil {
			return err
		}
	}
	return nil
}
