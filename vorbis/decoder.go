package vorbis

import (
	"errors"
	"fmt"

	"github.com/faide/goxiph/audio"
	"github.com/faide/goxiph/internal/bitio"
	"github.com/faide/goxiph/internal/mdct"
)

var (
	// ErrNotAudioPacket reports a packet that is a header rather than audio.
	ErrNotAudioPacket = errors.New("vorbis: not an audio packet")

	// ErrNoSetup reports decoding attempted before all three headers were supplied.
	ErrNoSetup = errors.New("vorbis: headers not yet decoded")
)

// Decoder turns Vorbis packets into PCM. Feed it the three header packets, then audio packets.
//
// A Decoder is not safe for concurrent use.
type Decoder struct {
	info  Info
	setup *Setup
	ready bool

	windows    *windowSet
	mdctShort  *mdct.Transform
	mdctLong   *mdct.Transform
	floorState []*floor1State

	// Per-channel working buffers, allocated once.
	spectrum [][]float32
	timeDom  [][]float32
	tail     [][]float32 // previous block from its centre onward, awaiting overlap
	work     []float32   // overlap-add scratch
	curve    []float32   // floor curve scratch

	noResidue   []bool
	doNotDecode []bool
	floorUnused []bool

	prevN   int
	hasPrev bool
}

// NewDecoder returns a decoder awaiting the three header packets.
func NewDecoder() *Decoder { return &Decoder{} }

// Info returns the identification header, valid once it has been supplied.
func (d *Decoder) Info() Info { return d.info }

// Format returns the PCM format the stream decodes to.
func (d *Decoder) Format() audio.Format { return d.info.Format() }

// Setup returns the decoded setup header, valid once it has been supplied.
func (d *Decoder) Setup() *Setup { return d.setup }

// SetInfo supplies the identification header.
func (d *Decoder) SetInfo(info Info) { d.info = info }

// SetSetup supplies the setup header and allocates the per-stream working buffers.
func (d *Decoder) SetSetup(s *Setup) error {
	if err := d.info.Format().Validate(); err != nil {
		return fmt.Errorf("%w: identification header missing or invalid", ErrNoSetup)
	}
	d.setup = s

	short, long := d.info.BlockSize0, d.info.BlockSize1
	d.windows = newWindowSet(short, long)

	var err error
	if d.mdctShort, err = mdct.New(short / 2); err != nil {
		return err
	}
	if d.mdctLong, err = mdct.New(long / 2); err != nil {
		return err
	}

	ch := d.info.Channels
	half := long / 2
	d.spectrum = make([][]float32, ch)
	d.timeDom = make([][]float32, ch)
	d.tail = make([][]float32, ch)
	for i := range ch {
		d.spectrum[i] = make([]float32, half)
		d.timeDom[i] = make([]float32, long)
		d.tail[i] = make([]float32, 0, long)
	}
	d.work = make([]float32, 2*long)
	d.curve = make([]float32, half)
	d.noResidue = make([]bool, ch)
	d.doNotDecode = make([]bool, ch)
	d.floorUnused = make([]bool, ch)

	// One floor scratch per channel, sized for the largest floor in the stream.
	maxValues := 0
	for _, f := range s.Floors {
		if f.Type == 1 {
			maxValues = max(maxValues, f.One.Values())
		}
	}
	d.floorState = make([]*floor1State, ch)
	for i := range ch {
		d.floorState[i] = newFloor1State(max(maxValues, 2), half)
	}

	d.ready = true
	return nil
}

// DecodePacket decodes one audio packet, appending PCM to out.
//
// The first audio packet primes the overlap and yields no samples, which is how the format works
// rather than a limitation here. out is grown as needed and its channel count must match the stream.
func (d *Decoder) DecodePacket(data []byte, out *audio.Buffer) error {
	if !d.ready {
		return ErrNoSetup
	}
	if len(data) == 0 {
		return fmt.Errorf("%w: empty packet", ErrNotAudioPacket)
	}

	r := bitio.NewLSBReader(data)
	packetType, err := r.Read(1)
	if err != nil || packetType != 0 {
		return fmt.Errorf("%w: type bit set", ErrNotAudioPacket)
	}

	modeBits := ilog(int32(len(d.setup.Modes) - 1))
	modeNum, err := r.Read(modeBits)
	if err != nil || int(modeNum) >= len(d.setup.Modes) {
		return fmt.Errorf("%w: mode %d of %d", ErrNotAudioPacket, modeNum, len(d.setup.Modes))
	}
	mode := d.setup.Modes[modeNum]

	n := d.info.BlockSize0
	if mode.BlockFlag {
		n = d.info.BlockSize1
	}

	prevLong, nextLong := true, true
	if mode.BlockFlag {
		p, err := r.ReadBool()
		if err != nil {
			return fmt.Errorf("%w: truncated at previous window flag", ErrNotAudioPacket)
		}
		nx, err := r.ReadBool()
		if err != nil {
			return fmt.Errorf("%w: truncated at next window flag", ErrNotAudioPacket)
		}
		prevLong, nextLong = p, nx
	}

	half := n / 2
	mapping := d.setup.Mappings[mode.Mapping]
	ch := d.info.Channels

	// Floor curves, in channel order. Vorbis I 4.3.2.
	for i := range ch {
		submap := mapping.Mux[i]
		floorNum := mapping.SubmapFloor[submap]
		fl := d.setup.Floors[floorNum]
		if fl.Type != 1 {
			return fmt.Errorf("vorbis: floor type %d synthesis is unimplemented", fl.Type)
		}
		used, err := fl.One.DecodePacket(r, d.setup.Codebooks, d.floorState[i])
		if err != nil {
			return err
		}
		d.floorUnused[i] = !used
		d.noResidue[i] = !used
	}

	// Coupled channels stand or fall together. Vorbis I 4.3.3.
	for _, cs := range mapping.CouplingSteps {
		if !d.noResidue[cs.Magnitude] || !d.noResidue[cs.Angle] {
			d.noResidue[cs.Magnitude] = false
			d.noResidue[cs.Angle] = false
		}
	}

	for i := range ch {
		clear(d.spectrum[i][:half])
	}

	// Residue vectors, in submap order. Vorbis I 4.3.4.
	for submap := range mapping.SubmapFloor {
		vectors := make([][]float32, 0, ch)
		skips := d.doNotDecode[:0]
		for j := range ch {
			if mapping.Mux[j] != submap {
				continue
			}
			vectors = append(vectors, d.spectrum[j][:half])
			skips = append(skips, d.noResidue[j])
		}
		if len(vectors) == 0 {
			continue
		}
		res := d.setup.Residues[mapping.SubmapResidue[submap]]
		if err := res.Decode(r, d.setup.Codebooks, vectors, skips); err != nil {
			return err
		}
	}

	// Inverse coupling, in reverse order. Vorbis I 4.3.5.
	for i := len(mapping.CouplingSteps) - 1; i >= 0; i-- {
		cs := mapping.CouplingSteps[i]
		mag := d.spectrum[cs.Magnitude][:half]
		ang := d.spectrum[cs.Angle][:half]
		for j := range mag {
			m, a := mag[j], ang[j]
			var newM, newA float32
			if m > 0 {
				if a > 0 {
					newM, newA = m, m-a
				} else {
					newA, newM = m, m+a
				}
			} else {
				if a > 0 {
					newM, newA = m, m+a
				} else {
					newA, newM = m, m-a
				}
			}
			mag[j], ang[j] = newM, newA
		}
	}

	// Floor curve times residue. Vorbis I 4.3.6.
	curve := d.curve[:half]
	for i := range ch {
		if d.floorUnused[i] {
			clear(d.spectrum[i][:half])
			continue
		}
		submap := mapping.Mux[i]
		fl := d.setup.Floors[mapping.SubmapFloor[submap]]
		fl.One.Synthesize(d.floorState[i], curve)
		spec := d.spectrum[i][:half]
		for j := range spec {
			spec[j] *= curve[j]
		}
	}

	// Inverse transform and window. Vorbis I 4.3.7.
	transform := d.mdctShort
	if mode.BlockFlag {
		transform = d.mdctLong
	}
	win := d.windows.forBlock(mode.BlockFlag, prevLong, nextLong)
	for i := range ch {
		block := d.timeDom[i][:n]
		if err := transform.Inverse(d.spectrum[i][:half], block); err != nil {
			return err
		}
		for j := range block {
			block[j] *= win[j]
		}
	}

	d.overlapAdd(n, out)
	return nil
}

// overlapAdd laps the current block onto the previous one and appends the finished samples to out.
// Vorbis I 4.3.8.
//
// The previous window's three-quarter point aligns with the current window's one-quarter point, so
// the current block starts at prevN/4 - n/4 relative to the previous block's centre. That offset is
// negative when a long block follows a short one, which is correct: the long window is zero over
// exactly the region that falls before the previous centre.
func (d *Decoder) overlapAdd(n int, out *audio.Buffer) {
	ch := d.info.Channels

	if !d.hasPrev {
		// The first block only primes the lap; the format returns no samples for it.
		for i := range ch {
			d.tail[i] = append(d.tail[i][:0], d.timeDom[i][n/2:n]...)
		}
		d.prevN = n
		d.hasPrev = true
		out.Resize(0)
		return
	}

	pn := d.prevN
	emit := pn/4 + n/4
	offset := pn/4 - n/4

	workLen := max(len(d.tail[0]), offset+n)
	if cap(d.work) < workLen {
		d.work = make([]float32, workLen)
	}

	base := out.Frames()
	out.Resize(base + emit)

	for i := range ch {
		work := d.work[:workLen]
		clear(work)
		copy(work, d.tail[i])

		block := d.timeDom[i][:n]
		for j := range block {
			k := offset + j
			if k >= 0 && k < workLen {
				work[k] += block[j]
			}
		}

		copy(out.Data[i][base:base+emit], work[:emit])
		d.tail[i] = append(d.tail[i][:0], work[emit:workLen]...)
	}

	d.prevN = n
}

// Reset clears the lapping state so the decoder can start a new logical stream.
func (d *Decoder) Reset() {
	d.hasPrev = false
	d.prevN = 0
	for i := range d.tail {
		d.tail[i] = d.tail[i][:0]
	}
}
