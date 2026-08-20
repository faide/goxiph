package flac

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/faide/goxiph/vorbiscomment"
)

// ErrBadStream reports data that is not a valid FLAC stream.
var ErrBadStream = errors.New("flac: malformed stream")

// Signature opens every native FLAC stream.
const Signature = "fLaC"

// Metadata block types. RFC 9639 section 8.1.
const (
	blockStreaminfo    = 0
	blockPadding       = 1
	blockApplication   = 2
	blockSeekTable     = 3
	blockVorbisComment = 4
	blockCuesheet      = 5
	blockPicture       = 6
	blockForbidden     = 127
)

// maxMetadataBlock bounds a metadata block. The length field is 24 bits, so an untrusted stream can
// otherwise ask for a 16 MiB allocation before the bytes prove they exist; this caps it at the
// format's own maximum and the read still fails if the data is short.
const maxMetadataBlock = 1 << 24

// StreamInfo is the mandatory first metadata block. RFC 9639 section 8.2.
type StreamInfo struct {
	MinBlockSize  int
	MaxBlockSize  int
	MinFrameSize  int
	MaxFrameSize  int
	SampleRate    int
	Channels      int
	BitsPerSample int
	TotalSamples  uint64
	MD5           [16]byte
}

// FixedBlockSize reports whether every frame but the last shares one block size.
func (s StreamInfo) FixedBlockSize() bool {
	return s.MinBlockSize == s.MaxBlockSize
}

// Validate rejects a stream info block that cannot describe a decodable stream.
func (s StreamInfo) Validate() error {
	if s.MinBlockSize < 16 || s.MaxBlockSize > 65535 {
		return fmt.Errorf("%w: block sizes %d..%d outside 16..65535", ErrBadStream, s.MinBlockSize, s.MaxBlockSize)
	}
	if s.MinBlockSize > s.MaxBlockSize {
		return fmt.Errorf("%w: min block size %d exceeds max %d", ErrBadStream, s.MinBlockSize, s.MaxBlockSize)
	}
	if s.Channels < 1 || s.Channels > 8 {
		return fmt.Errorf("%w: %d channels, want 1..8", ErrBadStream, s.Channels)
	}
	if s.BitsPerSample < 4 || s.BitsPerSample > 32 {
		return fmt.Errorf("%w: %d bits per sample, want 4..32", ErrBadStream, s.BitsPerSample)
	}
	return nil
}

// parseStreamInfo decodes the 34-byte stream info payload.
func parseStreamInfo(p []byte) (StreamInfo, error) {
	var s StreamInfo
	if len(p) < 34 {
		return s, fmt.Errorf("%w: stream info is %d bytes, want 34", ErrBadStream, len(p))
	}

	s.MinBlockSize = int(binary.BigEndian.Uint16(p[0:2]))
	s.MaxBlockSize = int(binary.BigEndian.Uint16(p[2:4]))
	s.MinFrameSize = int(p[4])<<16 | int(p[5])<<8 | int(p[6])
	s.MaxFrameSize = int(p[7])<<16 | int(p[8])<<8 | int(p[9])

	// Sample rate, channels and depth share bytes 10..12 as a 20/3/5 bit split.
	packed := uint32(p[10])<<16 | uint32(p[11])<<8 | uint32(p[12])
	s.SampleRate = int(packed >> 4)
	s.Channels = int((packed>>1)&0x07) + 1
	s.BitsPerSample = int((packed&0x01)<<4|uint32(p[13])>>4) + 1

	s.TotalSamples = uint64(p[13]&0x0f)<<32 | uint64(binary.BigEndian.Uint32(p[14:18]))
	copy(s.MD5[:], p[18:34])
	return s, s.Validate()
}

// A metadataBlock is one parsed metadata block.
type metadataBlock struct {
	kind    int
	last    bool
	payload []byte
}

// readMetadataBlock reads one metadata block header and its payload.
func readMetadataBlock(r io.Reader) (metadataBlock, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return metadataBlock{}, err
	}

	b := metadataBlock{
		last: head[0]&0x80 != 0,
		kind: int(head[0] & 0x7f),
	}
	if b.kind == blockForbidden {
		return b, fmt.Errorf("%w: metadata block type 127 is forbidden", ErrBadStream)
	}

	size := int(head[1])<<16 | int(head[2])<<8 | int(head[3])
	if size > maxMetadataBlock {
		return b, fmt.Errorf("%w: metadata block of %d bytes", ErrBadStream, size)
	}
	b.payload = make([]byte, size)
	if _, err := io.ReadFull(r, b.payload); err != nil {
		return b, fmt.Errorf("%w: metadata block truncated: %w", ErrBadStream, err)
	}
	return b, nil
}

// Metadata is everything read before the first audio frame.
type Metadata struct {
	StreamInfo StreamInfo
	Tags       vorbiscomment.Tags
}

// readMetadata consumes the signature and every metadata block.
func readMetadata(r io.Reader) (Metadata, error) {
	var m Metadata

	var sig [4]byte
	if _, err := io.ReadFull(r, sig[:]); err != nil {
		return m, fmt.Errorf("%w: reading signature: %w", ErrBadStream, err)
	}
	if string(sig[:]) != Signature {
		return m, fmt.Errorf("%w: signature %q, want %q", ErrBadStream, sig, Signature)
	}

	seenInfo := false
	for {
		b, err := readMetadataBlock(r)
		if err != nil {
			return m, err
		}
		switch b.kind {
		case blockStreaminfo:
			if seenInfo {
				return m, fmt.Errorf("%w: more than one stream info block", ErrBadStream)
			}
			if m.StreamInfo, err = parseStreamInfo(b.payload); err != nil {
				return m, err
			}
			seenInfo = true
		case blockVorbisComment:
			// FLAC carries the comment block without the trailing framing bit Vorbis requires.
			if m.Tags, err = vorbiscomment.Unmarshal(b.payload, false); err != nil {
				return m, err
			}
		}
		if b.last {
			break
		}
	}
	if !seenInfo {
		return m, fmt.Errorf("%w: no stream info block", ErrBadStream)
	}
	return m, nil
}
