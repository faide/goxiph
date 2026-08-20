package wav

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

// Encoder writes a RIFF/WAVE file.
//
// An Encoder is not safe for concurrent use.
type Encoder struct {
	w      io.Writer
	seeker io.WriteSeeker
	format Format

	riffSizeOffset int64
	dataSizeOffset int64
	dataBytes      int64
	frame          []byte
	closed         bool
}

// NewEncoder writes a WAVE header for format to w and stops at the first sample.
//
// RIFF stores the file and data lengths in its header, ahead of the samples they describe. When w is
// an io.WriteSeeker they are patched at Close; otherwise both are written as the streaming sentinel,
// which readers here and elsewhere take to mean "to the end of the file".
func NewEncoder(w io.Writer, format Format) (*Encoder, error) {
	if err := format.Validate(); err != nil {
		return nil, err
	}

	e := &Encoder{w: w, format: format, frame: make([]byte, format.BlockAlign())}
	if s, ok := w.(io.WriteSeeker); ok {
		e.seeker = s
	}
	if err := e.writeHeader(); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Encoder) writeHeader() error {
	fmtChunk := e.formatChunk()

	// RIFF size covers everything after the size field itself: the form type, the format chunk with
	// its header, and the data chunk header.
	riffSize := uint32(4 + 8 + len(fmtChunk) + 8)
	dataSize := uint32(0)
	if e.seeker == nil {
		riffSize, dataSize = unknownSize, unknownSize
	}

	var buf []byte
	buf = append(buf, idRIFF...)
	if e.seeker != nil {
		e.riffSizeOffset = 4
	}
	buf = binary.LittleEndian.AppendUint32(buf, riffSize)
	buf = append(buf, idWAVE...)

	buf = append(buf, idFmt...)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(fmtChunk)))
	buf = append(buf, fmtChunk...)

	buf = append(buf, idData...)
	if e.seeker != nil {
		e.dataSizeOffset = int64(len(buf))
	}
	buf = binary.LittleEndian.AppendUint32(buf, dataSize)

	_, err := e.w.Write(buf)
	return err
}

// formatChunk builds the format chunk payload, choosing the extensible layout when the classic one
// cannot describe the format.
func (e *Encoder) formatChunk() []byte {
	f := e.format
	tag := uint16(formatPCM)
	if f.Float {
		tag = formatIEEEFloat
	}

	var p []byte
	appendCommon := func(t uint16) {
		p = binary.LittleEndian.AppendUint16(p, t)
		p = binary.LittleEndian.AppendUint16(p, uint16(f.Channels))
		p = binary.LittleEndian.AppendUint32(p, uint32(f.SampleRate))
		p = binary.LittleEndian.AppendUint32(p, uint32(f.SampleRate*f.BlockAlign()))
		p = binary.LittleEndian.AppendUint16(p, uint16(f.BlockAlign()))
		p = binary.LittleEndian.AppendUint16(p, uint16(f.BitsPerSample))
	}

	if !f.needsExtensible() {
		appendCommon(tag)
		return p
	}

	appendCommon(formatExtensible)
	p = binary.LittleEndian.AppendUint16(p, 22) // extension size
	p = binary.LittleEndian.AppendUint16(p, uint16(f.BitsPerSample))
	p = binary.LittleEndian.AppendUint32(p, f.ChannelMask)
	p = binary.LittleEndian.AppendUint16(p, tag)
	p = append(p, extensibleGUIDTail[:]...)
	return p
}

// WriteInt32 appends frames of integer samples.
func (e *Encoder) WriteInt32(samples [][]int32) error {
	if e.closed {
		return fmt.Errorf("%w: write to a closed encoder", ErrBadFile)
	}
	if e.format.Float {
		return fmt.Errorf("%w: format holds float samples; use WriteFloat32", ErrBadFile)
	}
	n, err := frameCount(samples, e.format.Channels)
	if err != nil || n == 0 {
		return err
	}

	width := e.format.BytesPerSample()
	for i := range n {
		for ch := range samples {
			encodeInt(e.frame[ch*width:], samples[ch][i], width)
		}
		if _, err := e.w.Write(e.frame); err != nil {
			return err
		}
		e.dataBytes += int64(len(e.frame))
	}
	return nil
}

// WriteFloat32 appends frames of float samples, converting to the declared integer depth when the
// format is not itself float.
func (e *Encoder) WriteFloat32(samples [][]float32) error {
	if e.closed {
		return fmt.Errorf("%w: write to a closed encoder", ErrBadFile)
	}
	n, err := frameCount(samples, e.format.Channels)
	if err != nil || n == 0 {
		return err
	}

	width := e.format.BytesPerSample()
	scale := float64(int64(1) << (e.format.BitsPerSample - 1))
	for i := range n {
		for ch := range samples {
			b := e.frame[ch*width:]
			if e.format.Float {
				encodeFloat(b, samples[ch][i], width)
				continue
			}
			v := math.Round(float64(samples[ch][i]) * scale)
			v = math.Min(math.Max(v, -scale), scale-1)
			encodeInt(b, int32(v), width)
		}
		if _, err := e.w.Write(e.frame); err != nil {
			return err
		}
		e.dataBytes += int64(len(e.frame))
	}
	return nil
}

// frameCount checks that every channel is present and equal in length.
func frameCount[T any](samples [][]T, channels int) (int, error) {
	if len(samples) != channels {
		return 0, fmt.Errorf("%w: %d channels, format declares %d", ErrBadFile, len(samples), channels)
	}
	if len(samples) == 0 {
		return 0, nil
	}
	n := len(samples[0])
	for i, ch := range samples {
		if len(ch) != n {
			return 0, fmt.Errorf("%w: channel %d has %d frames, channel 0 has %d", ErrBadFile, i, len(ch), n)
		}
	}
	return n, nil
}

// Close writes the pad byte an odd-length data chunk needs and patches the header lengths.
func (e *Encoder) Close() error {
	if e.closed {
		return nil
	}
	e.closed = true

	// RIFF aligns chunks to even offsets, so odd sample data is followed by a pad byte that is not
	// counted in the chunk size.
	if e.dataBytes%2 == 1 {
		if _, err := e.w.Write([]byte{0}); err != nil {
			return err
		}
	}
	if e.seeker == nil {
		return nil
	}

	here, err := e.seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}

	var size [4]byte
	binary.LittleEndian.PutUint32(size[:], uint32(e.dataBytes))
	if _, err := e.seeker.Seek(e.dataSizeOffset, io.SeekStart); err != nil {
		return err
	}
	if _, err := e.seeker.Write(size[:]); err != nil {
		return err
	}

	binary.LittleEndian.PutUint32(size[:], uint32(here-8))
	if _, err := e.seeker.Seek(e.riffSizeOffset, io.SeekStart); err != nil {
		return err
	}
	if _, err := e.seeker.Write(size[:]); err != nil {
		return err
	}

	_, err = e.seeker.Seek(here, io.SeekStart)
	return err
}

// encodeInt writes one little-endian integer sample.
//
// Eight-bit samples are unsigned with a bias of 128, matching the decoder's inverse.
func encodeInt(b []byte, v int32, width int) {
	switch width {
	case 1:
		b[0] = byte(v + 128)
	case 2:
		binary.LittleEndian.PutUint16(b, uint16(v))
	case 3:
		b[0], b[1], b[2] = byte(v), byte(v>>8), byte(v>>16)
	default:
		binary.LittleEndian.PutUint32(b, uint32(v))
	}
}

// encodeFloat writes one IEEE sample.
func encodeFloat(b []byte, v float32, width int) {
	if width == 8 {
		binary.LittleEndian.PutUint64(b, math.Float64bits(float64(v)))
		return
	}
	binary.LittleEndian.PutUint32(b, math.Float32bits(v))
}
