// Package wav reads and writes RIFF/WAVE files.
//
// It covers integer PCM at 8, 16, 24 and 32 bits, IEEE float at 32 and 64 bits, and the
// WAVE_FORMAT_EXTENSIBLE header that carries a channel mask.
package wav

import (
	"errors"
	"fmt"
)

// ErrBadFile reports data that is not a well-formed RIFF/WAVE file.
var ErrBadFile = errors.New("wav: malformed file")

// Chunk and format identifiers.
const (
	idRIFF = "RIFF"
	idWAVE = "WAVE"
	idFmt  = "fmt "
	idData = "data"
)

// Format tags from the WAVE header.
const (
	formatPCM        = 0x0001
	formatIEEEFloat  = 0x0003
	formatExtensible = 0xFFFE
)

// extensibleGUIDTail is the fixed remainder of a WAVE_FORMAT_EXTENSIBLE subformat GUID; only the
// leading two bytes vary, and they hold the underlying format tag.
var extensibleGUIDTail = [14]byte{
	0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00,
	0x00, 0xAA, 0x00, 0x38, 0x9B, 0x71,
}

// unknownSize marks a data chunk whose length was not known when it was written. Writers that cannot
// seek back use it, and readers take it to mean "to the end of the file".
const unknownSize = 0xFFFFFFFF

// Format describes the samples in a WAVE file.
type Format struct {
	SampleRate    int
	Channels      int
	BitsPerSample int

	// Float reports IEEE floating point samples rather than integer PCM.
	Float bool

	// ChannelMask carries the speaker assignment from an extensible header, or zero when the file
	// has none.
	ChannelMask uint32
}

// BytesPerSample is the storage each sample occupies.
func (f Format) BytesPerSample() int { return (f.BitsPerSample + 7) / 8 }

// BlockAlign is the storage one frame occupies across all channels.
func (f Format) BlockAlign() int { return f.BytesPerSample() * f.Channels }

// Validate rejects a format no reader or writer here can handle.
func (f Format) Validate() error {
	if f.SampleRate <= 0 {
		return fmt.Errorf("%w: sample rate %d", ErrBadFile, f.SampleRate)
	}
	if f.Channels <= 0 || f.Channels > 255 {
		return fmt.Errorf("%w: %d channels", ErrBadFile, f.Channels)
	}
	if f.Float {
		if f.BitsPerSample != 32 && f.BitsPerSample != 64 {
			return fmt.Errorf("%w: %d-bit float, want 32 or 64", ErrBadFile, f.BitsPerSample)
		}
		return nil
	}
	switch f.BitsPerSample {
	case 8, 16, 24, 32:
		return nil
	default:
		return fmt.Errorf("%w: %d-bit integer PCM, want 8, 16, 24 or 32", ErrBadFile, f.BitsPerSample)
	}
}

// needsExtensible reports whether the format can only be expressed with an extensible header.
//
// More than two channels, a channel mask, or a depth that is not a whole number of bytes all require
// it; the classic 16-byte header cannot carry them unambiguously.
func (f Format) needsExtensible() bool {
	return f.Channels > 2 || f.ChannelMask != 0
}
