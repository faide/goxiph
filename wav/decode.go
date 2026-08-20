package wav

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// maxChunkScan bounds how far the reader will skip past unrecognised chunks before giving up, so a
// crafted file cannot make it walk forever.
const maxChunkScan = 4096

// maxFormatChunk bounds how much of a format chunk is held in memory. The extensible layout is 40
// bytes and nothing larger carries meaning here, so the rest is skipped rather than allocated: the
// size field is attacker-controlled and a file whose "fmt " length reads as 1.6 GB should not cause
// a 1.6 GB allocation.
const maxFormatChunk = 64

// Decoder reads samples from a RIFF/WAVE file.
//
// A Decoder is not safe for concurrent use.
type Decoder struct {
	br     *bufio.Reader
	format Format

	remaining int64 // bytes of sample data left, or -1 when the length was not declared
	frame     []byte
}

// NewDecoder reads the RIFF header and stops at the first sample.
func NewDecoder(r io.Reader) (*Decoder, error) {
	br := bufio.NewReaderSize(r, 1<<16)

	var header [12]byte
	if _, err := io.ReadFull(br, header[:]); err != nil {
		return nil, fmt.Errorf("%w: reading the RIFF header: %w", ErrBadFile, err)
	}
	if string(header[0:4]) != idRIFF {
		return nil, fmt.Errorf("%w: leading chunk is %q, want %q", ErrBadFile, header[0:4], idRIFF)
	}
	if string(header[8:12]) != idWAVE {
		return nil, fmt.Errorf("%w: form type is %q, want %q", ErrBadFile, header[8:12], idWAVE)
	}

	d := &Decoder{br: br, remaining: -1}
	seenFmt := false

	// Chunks may appear in any order and unknown ones are skipped, so the search runs until the
	// data chunk is found with a format already in hand.
	for scans := 0; ; scans++ {
		if scans > maxChunkScan {
			return nil, fmt.Errorf("%w: no data chunk within %d chunks", ErrBadFile, maxChunkScan)
		}

		id, size, err := readChunkHeader(br)
		if err != nil {
			return nil, err
		}

		switch id {
		case idFmt:
			want := min(int(size), maxFormatChunk)
			payload := make([]byte, want)
			if _, err := io.ReadFull(br, payload); err != nil {
				return nil, fmt.Errorf("%w: reading the format chunk: %w", ErrBadFile, err)
			}
			if d.format, err = parseFormat(payload); err != nil {
				return nil, err
			}
			seenFmt = true
			if rest := int(size) - want; rest > 0 {
				if _, err := br.Discard(rest); err != nil {
					return nil, fmt.Errorf("%w: skipping the rest of the format chunk: %w", ErrBadFile, err)
				}
			}
			if err := skipPad(br, size); err != nil {
				return nil, err
			}

		case idData:
			if !seenFmt {
				return nil, fmt.Errorf("%w: data chunk precedes the format chunk", ErrBadFile)
			}
			if size != unknownSize && size != 0 {
				d.remaining = int64(size)
			}
			d.frame = make([]byte, d.format.BlockAlign())
			return d, nil

		default:
			if err := skipChunk(br, size); err != nil {
				return nil, err
			}
		}
	}
}

// readChunkHeader reads a four-character identifier and its payload size.
func readChunkHeader(br *bufio.Reader) (string, uint32, error) {
	var head [8]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return "", 0, fmt.Errorf("%w: file ends before the data chunk", ErrBadFile)
		}
		return "", 0, err
	}
	return string(head[0:4]), binary.LittleEndian.Uint32(head[4:8]), nil
}

// skipChunk discards a chunk payload and its pad byte.
func skipChunk(br *bufio.Reader, size uint32) error {
	if size == unknownSize {
		return fmt.Errorf("%w: chunk of unknown size before the data chunk", ErrBadFile)
	}
	if _, err := br.Discard(int(size)); err != nil {
		return fmt.Errorf("%w: skipping a chunk: %w", ErrBadFile, err)
	}
	return skipPad(br, size)
}

// skipPad consumes the pad byte that follows an odd-length chunk.
//
// RIFF aligns every chunk to an even offset, so a chunk with an odd size is followed by a byte that
// is not part of it. Missing this desynchronises every later chunk.
func skipPad(br *bufio.Reader, size uint32) error {
	if size%2 == 0 {
		return nil
	}
	if _, err := br.Discard(1); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// parseFormat decodes a format chunk payload.
func parseFormat(p []byte) (Format, error) {
	var f Format
	if len(p) < 16 {
		return f, fmt.Errorf("%w: format chunk is %d bytes, want at least 16", ErrBadFile, len(p))
	}

	tag := binary.LittleEndian.Uint16(p[0:2])
	f.Channels = int(binary.LittleEndian.Uint16(p[2:4]))
	f.SampleRate = int(binary.LittleEndian.Uint32(p[4:8]))
	f.BitsPerSample = int(binary.LittleEndian.Uint16(p[14:16]))

	if tag == formatExtensible {
		if len(p) < 40 {
			return f, fmt.Errorf("%w: extensible format chunk is %d bytes, want 40", ErrBadFile, len(p))
		}
		f.ChannelMask = binary.LittleEndian.Uint32(p[20:24])

		guid := p[24:40]
		if [14]byte(guid[2:16]) != extensibleGUIDTail {
			return f, fmt.Errorf("%w: unrecognised subformat GUID %x", ErrBadFile, guid)
		}
		tag = binary.LittleEndian.Uint16(guid[0:2])
	}

	switch tag {
	case formatPCM:
		f.Float = false
	case formatIEEEFloat:
		f.Float = true
	default:
		return f, fmt.Errorf("%w: format tag %#04x is unsupported", ErrBadFile, tag)
	}
	return f, f.Validate()
}

// Format returns the sample format the file declares.
func (d *Decoder) Format() Format { return d.format }

// Frames reports the number of frames the data chunk declares, or -1 when it did not.
func (d *Decoder) Frames() int64 {
	if d.remaining < 0 {
		return -1
	}
	return d.remaining / int64(d.format.BlockAlign())
}

// readFrame loads one frame of raw bytes, returning io.EOF at the end of the data.
func (d *Decoder) readFrame() error {
	if d.remaining == 0 {
		return io.EOF
	}
	if _, err := io.ReadFull(d.br, d.frame); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return io.EOF
		}
		return err
	}
	if d.remaining > 0 {
		d.remaining -= int64(len(d.frame))
		if d.remaining < 0 {
			d.remaining = 0
		}
	}
	return nil
}

// ReadInt32 fills dst with up to len(dst[0]) frames of integer samples, returning how many it read.
//
// Float files are rejected: converting them here would silently pick a scale the caller did not
// choose. Use ReadFloat32 for those.
func (d *Decoder) ReadInt32(dst [][]int32) (int, error) {
	if d.format.Float {
		return 0, fmt.Errorf("%w: file holds float samples; use ReadFloat32", ErrBadFile)
	}
	if len(dst) != d.format.Channels {
		return 0, fmt.Errorf("%w: %d destination channels, file has %d", ErrBadFile, len(dst), d.format.Channels)
	}
	if len(dst) == 0 {
		return 0, nil
	}

	width := d.format.BytesPerSample()
	for i := range dst[0] {
		if err := d.readFrame(); err != nil {
			if errors.Is(err, io.EOF) {
				return i, nil
			}
			return i, err
		}
		for ch := range dst {
			dst[ch][i] = decodeInt(d.frame[ch*width:], width)
		}
	}
	return len(dst[0]), nil
}

// ReadFloat32 fills dst with up to len(dst[0]) frames, normalising integer samples to [-1, 1).
func (d *Decoder) ReadFloat32(dst [][]float32) (int, error) {
	if len(dst) != d.format.Channels {
		return 0, fmt.Errorf("%w: %d destination channels, file has %d", ErrBadFile, len(dst), d.format.Channels)
	}
	if len(dst) == 0 {
		return 0, nil
	}

	width := d.format.BytesPerSample()
	scale := float32(int64(1) << (d.format.BitsPerSample - 1))
	for i := range dst[0] {
		if err := d.readFrame(); err != nil {
			if errors.Is(err, io.EOF) {
				return i, nil
			}
			return i, err
		}
		for ch := range dst {
			b := d.frame[ch*width:]
			if d.format.Float {
				dst[ch][i] = decodeFloat(b, width)
			} else {
				dst[ch][i] = float32(decodeInt(b, width)) / scale
			}
		}
	}
	return len(dst[0]), nil
}

// decodeInt reads one little-endian integer sample.
//
// Eight-bit WAVE samples are unsigned with a bias of 128, unlike every wider depth, which is signed.
// Reading them as signed puts the waveform half a scale out of place.
func decodeInt(b []byte, width int) int32 {
	switch width {
	case 1:
		return int32(b[0]) - 128
	case 2:
		return int32(int16(binary.LittleEndian.Uint16(b)))
	case 3:
		v := int32(b[0]) | int32(b[1])<<8 | int32(b[2])<<16
		// Sign-extend from bit 23.
		if v&0x800000 != 0 {
			v |= ^0xFFFFFF
		}
		return v
	default:
		return int32(binary.LittleEndian.Uint32(b))
	}
}

// decodeFloat reads one IEEE sample.
func decodeFloat(b []byte, width int) float32 {
	if width == 8 {
		return float32(math.Float64frombits(binary.LittleEndian.Uint64(b)))
	}
	return math.Float32frombits(binary.LittleEndian.Uint32(b))
}

// DecodeAllInt32 reads a whole integer WAVE file into planar channels.
func DecodeAllInt32(r io.Reader) ([][]int32, Format, error) {
	d, err := NewDecoder(r)
	if err != nil {
		return nil, Format{}, err
	}

	out := make([][]int32, d.format.Channels)
	chunk := make([][]int32, d.format.Channels)
	for i := range chunk {
		chunk[i] = make([]int32, 4096)
	}

	for {
		n, err := d.ReadInt32(chunk)
		if err != nil {
			return out, d.format, err
		}
		for ch := range out {
			out[ch] = append(out[ch], chunk[ch][:n]...)
		}
		if n < len(chunk[0]) {
			return out, d.format, nil
		}
	}
}
