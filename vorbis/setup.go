package vorbis

import (
	"errors"
	"fmt"

	"github.com/faide/goxiph/internal/bitio"
)

// ErrBadSetup reports a setup header that violates the specification.
var ErrBadSetup = errors.New("vorbis: invalid setup header")

// Mapping assigns channels to submaps and describes the coupling applied between them.
type Mapping struct {
	CouplingSteps []CouplingStep
	Mux           []int // per channel, which submap it uses
	SubmapFloor   []int
	SubmapResidue []int
}

// CouplingStep is one magnitude/angle channel pair. Vorbis I 4.2.4.
type CouplingStep struct {
	Magnitude int
	Angle     int
}

// Mode selects a block size, a window and a mapping for an audio packet.
type Mode struct {
	BlockFlag bool
	Mapping   int
}

// Setup is the decoded third header packet.
type Setup struct {
	Codebooks []*Codebook
	Floors    []*Floor
	Residues  []*Residue
	Mappings  []*Mapping
	Modes     []*Mode
}

// ParseSetup decodes the setup header, the third packet of a Vorbis stream. Vorbis I 4.2.4.
func ParseSetup(data []byte, info Info) (*Setup, error) {
	payload, err := headerPayload(data, packetSetup)
	if err != nil {
		return nil, err
	}
	r := bitio.NewLSBReader(payload)
	s := &Setup{}

	if err := s.readCodebooks(r); err != nil {
		return nil, err
	}
	if err := s.readTimeTransforms(r); err != nil {
		return nil, err
	}
	if err := s.readFloors(r); err != nil {
		return nil, err
	}
	if err := s.readResidues(r); err != nil {
		return nil, err
	}
	if err := s.readMappings(r, info); err != nil {
		return nil, err
	}
	if err := s.readModes(r); err != nil {
		return nil, err
	}

	framing, err := r.Read(1)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at framing bit", ErrBadSetup)
	}
	if framing == 0 {
		return nil, fmt.Errorf("%w: framing bit clear", ErrBadSetup)
	}
	return s, nil
}

func (s *Setup) readCodebooks(r *bitio.LSBReader) error {
	n, err := r.Read(8)
	if err != nil {
		return fmt.Errorf("%w: truncated at codebook count", ErrBadSetup)
	}
	s.Codebooks = make([]*Codebook, int(n)+1)
	for i := range s.Codebooks {
		c, err := readCodebook(r)
		if err != nil {
			return fmt.Errorf("codebook %d: %w", i, err)
		}
		s.Codebooks[i] = c
	}
	return nil
}

// readTimeTransforms consumes the time domain transform list, whose values the specification
// requires to be zero. It is a placeholder field that never carried anything.
func (s *Setup) readTimeTransforms(r *bitio.LSBReader) error {
	n, err := r.Read(6)
	if err != nil {
		return fmt.Errorf("%w: truncated at time transform count", ErrBadSetup)
	}
	for i := range int(n) + 1 {
		v, err := r.Read(16)
		if err != nil {
			return fmt.Errorf("%w: truncated at time transform %d", ErrBadSetup, i)
		}
		if v != 0 {
			return fmt.Errorf("%w: time transform %d is %#04x, want 0", ErrBadSetup, i, v)
		}
	}
	return nil
}

func (s *Setup) readFloors(r *bitio.LSBReader) error {
	n, err := r.Read(6)
	if err != nil {
		return fmt.Errorf("%w: truncated at floor count", ErrBadSetup)
	}
	s.Floors = make([]*Floor, int(n)+1)
	for i := range s.Floors {
		f, err := readFloor(r, s.Codebooks)
		if err != nil {
			return fmt.Errorf("floor %d: %w", i, err)
		}
		s.Floors[i] = f
	}
	return nil
}

func (s *Setup) readResidues(r *bitio.LSBReader) error {
	n, err := r.Read(6)
	if err != nil {
		return fmt.Errorf("%w: truncated at residue count", ErrBadSetup)
	}
	s.Residues = make([]*Residue, int(n)+1)
	for i := range s.Residues {
		res, err := readResidue(r, s.Codebooks)
		if err != nil {
			return fmt.Errorf("residue %d: %w", i, err)
		}
		s.Residues[i] = res
	}
	return nil
}

func (s *Setup) readMappings(r *bitio.LSBReader, info Info) error {
	n, err := r.Read(6)
	if err != nil {
		return fmt.Errorf("%w: truncated at mapping count", ErrBadSetup)
	}
	s.Mappings = make([]*Mapping, int(n)+1)
	for i := range s.Mappings {
		m, err := s.readMapping(r, info)
		if err != nil {
			return fmt.Errorf("mapping %d: %w", i, err)
		}
		s.Mappings[i] = m
	}
	return nil
}

// readMapping decodes one mapping. Vorbis I 4.2.4, mapping type 0 is the only one defined.
func (s *Setup) readMapping(r *bitio.LSBReader, info Info) (*Mapping, error) {
	t, err := r.Read(16)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at mapping type", ErrBadSetup)
	}
	if t != 0 {
		return nil, fmt.Errorf("%w: mapping type %d, want 0", ErrBadSetup, t)
	}

	m := &Mapping{}
	submaps := 1
	hasSubmaps, err := r.ReadBool()
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at submap flag", ErrBadSetup)
	}
	if hasSubmaps {
		v, err := r.Read(4)
		if err != nil {
			return nil, fmt.Errorf("%w: truncated at submap count", ErrBadSetup)
		}
		submaps = int(v) + 1
	}

	coupled, err := r.ReadBool()
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at coupling flag", ErrBadSetup)
	}
	if coupled {
		v, err := r.Read(8)
		if err != nil {
			return nil, fmt.Errorf("%w: truncated at coupling step count", ErrBadSetup)
		}
		steps := int(v) + 1
		bits := ilog(int32(info.Channels - 1))
		m.CouplingSteps = make([]CouplingStep, steps)
		for i := range m.CouplingSteps {
			mag, err := r.Read(bits)
			if err != nil {
				return nil, fmt.Errorf("%w: truncated at coupling %d magnitude", ErrBadSetup, i)
			}
			ang, err := r.Read(bits)
			if err != nil {
				return nil, fmt.Errorf("%w: truncated at coupling %d angle", ErrBadSetup, i)
			}
			if mag == ang || int(mag) >= info.Channels || int(ang) >= info.Channels {
				return nil, fmt.Errorf("%w: coupling %d pairs channels %d and %d", ErrBadSetup, i, mag, ang)
			}
			m.CouplingSteps[i] = CouplingStep{Magnitude: int(mag), Angle: int(ang)}
		}
	}

	reserved, err := r.Read(2)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at reserved field", ErrBadSetup)
	}
	if reserved != 0 {
		return nil, fmt.Errorf("%w: reserved field is %d, want 0", ErrBadSetup, reserved)
	}

	m.Mux = make([]int, info.Channels)
	if submaps > 1 {
		for i := range m.Mux {
			v, err := r.Read(4)
			if err != nil {
				return nil, fmt.Errorf("%w: truncated at channel %d mux", ErrBadSetup, i)
			}
			if int(v) >= submaps {
				return nil, fmt.Errorf("%w: channel %d uses submap %d of %d", ErrBadSetup, i, v, submaps)
			}
			m.Mux[i] = int(v)
		}
	}

	m.SubmapFloor = make([]int, submaps)
	m.SubmapResidue = make([]int, submaps)
	for i := range submaps {
		if _, err := r.Read(8); err != nil { // unused placeholder
			return nil, fmt.Errorf("%w: truncated at submap %d placeholder", ErrBadSetup, i)
		}
		f, err := r.Read(8)
		if err != nil {
			return nil, fmt.Errorf("%w: truncated at submap %d floor", ErrBadSetup, i)
		}
		if int(f) >= len(s.Floors) {
			return nil, fmt.Errorf("%w: submap %d floor %d, only %d floors", ErrBadSetup, i, f, len(s.Floors))
		}
		res, err := r.Read(8)
		if err != nil {
			return nil, fmt.Errorf("%w: truncated at submap %d residue", ErrBadSetup, i)
		}
		if int(res) >= len(s.Residues) {
			return nil, fmt.Errorf("%w: submap %d residue %d, only %d residues", ErrBadSetup, i, res, len(s.Residues))
		}
		m.SubmapFloor[i] = int(f)
		m.SubmapResidue[i] = int(res)
	}
	return m, nil
}

func (s *Setup) readModes(r *bitio.LSBReader) error {
	n, err := r.Read(6)
	if err != nil {
		return fmt.Errorf("%w: truncated at mode count", ErrBadSetup)
	}
	s.Modes = make([]*Mode, int(n)+1)
	for i := range s.Modes {
		blockFlag, err := r.ReadBool()
		if err != nil {
			return fmt.Errorf("%w: truncated at mode %d block flag", ErrBadSetup, i)
		}
		windowType, err := r.Read(16)
		if err != nil {
			return fmt.Errorf("%w: truncated at mode %d window type", ErrBadSetup, i)
		}
		transformType, err := r.Read(16)
		if err != nil {
			return fmt.Errorf("%w: truncated at mode %d transform type", ErrBadSetup, i)
		}
		mapping, err := r.Read(8)
		if err != nil {
			return fmt.Errorf("%w: truncated at mode %d mapping", ErrBadSetup, i)
		}
		if windowType != 0 || transformType != 0 {
			return fmt.Errorf("%w: mode %d has window %d transform %d, both must be 0",
				ErrBadSetup, i, windowType, transformType)
		}
		if int(mapping) >= len(s.Mappings) {
			return fmt.Errorf("%w: mode %d uses mapping %d of %d", ErrBadSetup, i, mapping, len(s.Mappings))
		}
		s.Modes[i] = &Mode{BlockFlag: blockFlag, Mapping: int(mapping)}
	}
	return nil
}
