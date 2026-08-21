package opus

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/faide/goxiph/vorbiscomment"
)

// ErrBadHeader reports a header packet that violates RFC 7845 section 5.
var ErrBadHeader = errors.New("opus: invalid header")

// Header magic strings. Both open with "Op", which is not a valid TOC byte sequence, so a header can
// never be mistaken for audio.
const (
	headMagic = "OpusHead"
	tagsMagic = "OpusTags"
)

// maxSupportedVersion is the highest encapsulation version treated as compatible.
//
// RFC 7845 section 5.1 splits the version into major and minor halves, and a stream is compatible
// when its major half matches, which is every value up to 15.
const maxSupportedVersion = 15

// Channel mapping families. RFC 7845 section 5.1.1.
const (
	// MappingFamilyMono is one or two channels in the Vorbis order, with no mapping table.
	MappingFamilyMono = 0
	// MappingFamilySurround is 1 to 8 channels in the Vorbis order, with a mapping table.
	MappingFamilySurround = 1
	// MappingFamilyAmbisonics is ambisonic components in channel-number order, optionally followed
	// by a stereo pair, with the same mapping table as family 1. RFC 8486 section 3.1.
	MappingFamilyAmbisonics = 2
	// MappingFamilyAmbisonicsMatrix is ambisonics reconstructed by multiplying the decoded channels
	// by a matrix the header carries in place of a channel mapping. RFC 8486 section 3.2.
	MappingFamilyAmbisonicsMatrix = 3
	// MappingFamilyDiscrete is unidentified channels, with a mapping table.
	MappingFamilyDiscrete = 255
)

// Head is the Opus identification header.
type Head struct {
	Version       int
	Channels      int
	PreSkip       int
	InputRate     int
	OutputGain    int16 // Q7.8 decibels
	MappingFamily int

	// StreamCount and CoupledCount describe how the channels are carried, and ChannelMapping maps
	// each output channel onto a decoded one. Families other than mono carry them in the header.
	StreamCount    int
	CoupledCount   int
	ChannelMapping []byte

	// DemixingMatrix is carried instead of a channel mapping by family 3, and is empty otherwise.
	// Its entries are signed and in row-major order, one row per output channel.
	DemixingMatrix []int16

	// Undecodable marks a header whose channel mapping family this implementation does not know.
	// RFC 8486 section 5.2: nothing past the first nineteen octets may be relied on, and the
	// packets must not be decoded.
	Undecodable bool
}

// PreSkipSamples is the audio to discard from the start of the stream, at the 48 kHz decode rate.
//
// Every Opus encoder introduces delay, and this is how much. Failing to drop it puts a click at the
// start of the stream and shifts every later sample position.
func (h Head) PreSkipSamples() int { return h.PreSkip }

// ParseHead decodes an OpusHead packet.
func ParseHead(data []byte) (Head, error) {
	var h Head
	if len(data) < 19 {
		return h, fmt.Errorf("%w: identification header is %d bytes, want at least 19", ErrBadHeader, len(data))
	}
	if string(data[0:8]) != headMagic {
		return h, fmt.Errorf("%w: magic %q, want %q", ErrBadHeader, data[0:8], headMagic)
	}

	h.Version = int(data[8])
	if h.Version > maxSupportedVersion {
		return h, fmt.Errorf("%w: encapsulation version %d is incompatible", ErrBadHeader, h.Version)
	}

	h.Channels = int(data[9])
	if h.Channels == 0 {
		return h, fmt.Errorf("%w: zero channels", ErrBadHeader)
	}
	h.PreSkip = int(binary.LittleEndian.Uint16(data[10:12]))
	h.InputRate = int(binary.LittleEndian.Uint32(data[12:16]))
	h.OutputGain = int16(binary.LittleEndian.Uint16(data[16:18]))
	h.MappingFamily = int(data[18])

	if h.MappingFamily == MappingFamilyMono {
		if h.Channels > 2 {
			return h, fmt.Errorf("%w: mapping family 0 with %d channels, want 1 or 2", ErrBadHeader, h.Channels)
		}
		// The mapping is implicit: one stream, coupled when stereo.
		h.StreamCount = 1
		h.CoupledCount = h.Channels - 1
		h.ChannelMapping = make([]byte, h.Channels)
		for i := range h.ChannelMapping {
			h.ChannelMapping[i] = byte(i)
		}
		return h, nil
	}

	switch h.MappingFamily {
	case MappingFamilySurround, MappingFamilyAmbisonics, MappingFamilyDiscrete,
		MappingFamilyAmbisonicsMatrix:
	default:
		// A family this implementation does not know may lay its mapping table out in any shape, so
		// reading one would be guessing. Everything up to here is fixed by the format and stands.
		// RFC 8486 section 5.2.
		h.Undecodable = true
		return h, nil
	}

	if len(data) < 21 {
		return h, fmt.Errorf("%w: mapping table wants at least 21 bytes, header holds %d",
			ErrBadHeader, len(data))
	}
	h.StreamCount = int(data[19])
	h.CoupledCount = int(data[20])
	if h.StreamCount == 0 {
		return h, fmt.Errorf("%w: zero streams", ErrBadHeader)
	}
	if h.CoupledCount > h.StreamCount {
		return h, fmt.Errorf("%w: %d coupled streams of %d total", ErrBadHeader, h.CoupledCount, h.StreamCount)
	}
	decoded := h.StreamCount + h.CoupledCount
	if decoded > 255 {
		return h, fmt.Errorf("%w: %d decoded channels exceed 255", ErrBadHeader, decoded)
	}

	if h.MappingFamily == MappingFamilyAmbisonicsMatrix {
		// The matrix stands in for the channel mapping: every output channel is a combination of
		// the decoded ones rather than a choice among them.
		want := 2 * decoded * h.Channels
		if len(data) < 21+want {
			return h, fmt.Errorf("%w: demixing matrix wants %d bytes, header holds %d",
				ErrBadHeader, 21+want, len(data))
		}
		h.DemixingMatrix = make([]int16, decoded*h.Channels)
		for i := range h.DemixingMatrix {
			h.DemixingMatrix[i] = int16(binary.LittleEndian.Uint16(data[21+2*i:]))
		}
		return h, nil
	}

	if len(data) < 21+h.Channels {
		return h, fmt.Errorf("%w: mapping table wants %d bytes, header holds %d",
			ErrBadHeader, 21+h.Channels, len(data))
	}
	h.ChannelMapping = make([]byte, h.Channels)
	copy(h.ChannelMapping, data[21:21+h.Channels])
	for i, m := range h.ChannelMapping {
		// 255 marks a silent channel; anything else must name a decoded one.
		if m != 255 && int(m) >= decoded {
			return h, fmt.Errorf("%w: channel %d maps to %d, only %d decoded channels exist",
				ErrBadHeader, i, m, decoded)
		}
	}
	return h, nil
}

// AppendTo appends the encoded identification header to dst.
func (h Head) AppendTo(dst []byte) ([]byte, error) {
	if h.Channels < 1 || h.Channels > 255 {
		return dst, fmt.Errorf("%w: %d channels", ErrBadHeader, h.Channels)
	}

	dst = append(dst, headMagic...)
	dst = append(dst, byte(h.Version), byte(h.Channels))
	dst = binary.LittleEndian.AppendUint16(dst, uint16(h.PreSkip))
	dst = binary.LittleEndian.AppendUint32(dst, uint32(h.InputRate))
	dst = binary.LittleEndian.AppendUint16(dst, uint16(h.OutputGain))
	dst = append(dst, byte(h.MappingFamily))

	if h.MappingFamily == MappingFamilyMono {
		return dst, nil
	}
	if len(h.ChannelMapping) != h.Channels {
		return dst, fmt.Errorf("%w: mapping table has %d entries for %d channels",
			ErrBadHeader, len(h.ChannelMapping), h.Channels)
	}
	dst = append(dst, byte(h.StreamCount), byte(h.CoupledCount))
	return append(dst, h.ChannelMapping...), nil
}

// ParseTags decodes an OpusTags packet.
func ParseTags(data []byte) (vorbiscomment.Tags, error) {
	if len(data) < 8 {
		return vorbiscomment.Tags{}, fmt.Errorf("%w: comment header is %d bytes, want at least 8",
			ErrBadHeader, len(data))
	}
	if string(data[0:8]) != tagsMagic {
		return vorbiscomment.Tags{}, fmt.Errorf("%w: magic %q, want %q", ErrBadHeader, data[0:8], tagsMagic)
	}
	// As in FLAC, the block carries no trailing framing bit; only Vorbis requires one.
	return vorbiscomment.Unmarshal(data[8:], false)
}

// AppendTags appends an encoded comment header to dst.
func AppendTags(dst []byte, t vorbiscomment.Tags) []byte {
	dst = append(dst, tagsMagic...)
	return t.AppendTo(dst, false)
}

// IsHead reports whether a packet is an identification header.
func IsHead(data []byte) bool {
	return len(data) >= 8 && string(data[0:8]) == headMagic
}

// IsTags reports whether a packet is a comment header.
func IsTags(data []byte) bool {
	return len(data) >= 8 && string(data[0:8]) == tagsMagic
}
