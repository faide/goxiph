// Package vorbis implements the Vorbis I audio codec.
package vorbis

import (
	"errors"
	"fmt"
	"math/bits"

	"github.com/faide/goxiph/audio"
	"github.com/faide/goxiph/internal/bitio"
	"github.com/faide/goxiph/vorbiscomment"
)

// Packet type codes. Vorbis I 4.2.1: audio packets have bit 0 clear, headers have it set.
const (
	packetAudio      = 0x00
	packetIdent      = 0x01
	packetComment    = 0x03
	packetSetup      = 0x05
	headerSignature  = "vorbis"
	minBlockSizeExp  = 6  // 64 samples
	maxBlockSizeExp  = 13 // 8192 samples
	identPayloadBits = 30 * 8
)

var (
	// ErrNotVorbis reports a packet without the Vorbis header signature.
	ErrNotVorbis = errors.New("vorbis: not a Vorbis header packet")

	// ErrBadHeader reports a header that parses but violates the specification.
	ErrBadHeader = errors.New("vorbis: invalid header")
)

// Info is the Vorbis identification header.
type Info struct {
	Version        uint32
	Channels       int
	SampleRate     int
	BitrateMaximum int32
	BitrateNominal int32
	BitrateMinimum int32
	BlockSize0     int // short block, in samples
	BlockSize1     int // long block, in samples
}

// Format returns the PCM format the stream decodes to.
func (i Info) Format() audio.Format {
	return audio.Format{SampleRate: i.SampleRate, Channels: i.Channels}
}

// headerPayload strips the packet type byte and the "vorbis" signature, checking both.
func headerPayload(data []byte, want byte) ([]byte, error) {
	const prefix = 1 + len(headerSignature)
	if len(data) < prefix {
		return nil, fmt.Errorf("%w: %d bytes, need at least %d", ErrNotVorbis, len(data), prefix)
	}
	if string(data[1:prefix]) != headerSignature {
		return nil, fmt.Errorf("%w: signature %q", ErrNotVorbis, data[1:prefix])
	}
	if data[0] != want {
		return nil, fmt.Errorf("%w: packet type %#02x, want %#02x", ErrNotVorbis, data[0], want)
	}
	return data[prefix:], nil
}

// ParseInfo decodes the identification header, the first packet of a Vorbis stream.
func ParseInfo(data []byte) (Info, error) {
	var info Info
	payload, err := headerPayload(data, packetIdent)
	if err != nil {
		return info, err
	}

	r := bitio.NewLSBReader(payload)
	read := func(n uint) uint32 {
		v, e := r.Read(n)
		if e != nil && err == nil {
			err = fmt.Errorf("%w: identification header truncated", ErrBadHeader)
		}
		return v
	}

	info.Version = read(32)
	info.Channels = int(read(8))
	info.SampleRate = int(read(32))
	info.BitrateMaximum = int32(read(32))
	info.BitrateNominal = int32(read(32))
	info.BitrateMinimum = int32(read(32))
	exp0 := uint(read(4))
	exp1 := uint(read(4))
	framing := read(1)
	if err != nil {
		return info, err
	}

	// Vorbis I 4.2.2 lists these as the conditions under which a stream is undecodable.
	if info.Version != 0 {
		return info, fmt.Errorf("%w: version %d, want 0", ErrBadHeader, info.Version)
	}
	if info.Channels == 0 {
		return info, fmt.Errorf("%w: zero channels", ErrBadHeader)
	}
	if info.SampleRate == 0 {
		return info, fmt.Errorf("%w: zero sample rate", ErrBadHeader)
	}
	if framing == 0 {
		return info, fmt.Errorf("%w: framing bit clear", ErrBadHeader)
	}
	if exp0 < minBlockSizeExp || exp0 > maxBlockSizeExp {
		return info, fmt.Errorf("%w: short block exponent %d outside 6..13", ErrBadHeader, exp0)
	}
	if exp1 < minBlockSizeExp || exp1 > maxBlockSizeExp {
		return info, fmt.Errorf("%w: long block exponent %d outside 6..13", ErrBadHeader, exp1)
	}
	if exp0 > exp1 {
		return info, fmt.Errorf("%w: short block %d exceeds long block %d", ErrBadHeader, 1<<exp0, 1<<exp1)
	}
	info.BlockSize0 = 1 << exp0
	info.BlockSize1 = 1 << exp1
	return info, nil
}

// AppendTo appends the encoded identification header to dst.
func (i Info) AppendTo(dst []byte) ([]byte, error) {
	if i.Channels <= 0 || i.Channels > 255 {
		return dst, fmt.Errorf("%w: %d channels", ErrBadHeader, i.Channels)
	}
	if i.SampleRate <= 0 {
		return dst, fmt.Errorf("%w: sample rate %d", ErrBadHeader, i.SampleRate)
	}
	exp0, err := blockExp(i.BlockSize0)
	if err != nil {
		return dst, err
	}
	exp1, err := blockExp(i.BlockSize1)
	if err != nil {
		return dst, err
	}
	if exp0 > exp1 {
		return dst, fmt.Errorf("%w: short block %d exceeds long block %d", ErrBadHeader, i.BlockSize0, i.BlockSize1)
	}

	w := bitio.NewLSBWriter()
	_ = w.WriteBytes(append([]byte{packetIdent}, headerSignature...))
	_ = w.Write(i.Version, 32)
	_ = w.Write(uint32(i.Channels), 8)
	_ = w.Write(uint32(i.SampleRate), 32)
	_ = w.Write(uint32(i.BitrateMaximum), 32)
	_ = w.Write(uint32(i.BitrateNominal), 32)
	_ = w.Write(uint32(i.BitrateMinimum), 32)
	_ = w.Write(uint32(exp0), 4)
	_ = w.Write(uint32(exp1), 4)
	_ = w.Write(1, 1)
	return append(dst, w.Bytes()...), nil
}

// blockExp converts a block size to its exponent, rejecting sizes the format cannot express.
func blockExp(size int) (uint, error) {
	if size <= 0 || bits.OnesCount(uint(size)) != 1 {
		return 0, fmt.Errorf("%w: block size %d is not a power of two", ErrBadHeader, size)
	}
	exp := uint(bits.TrailingZeros(uint(size)))
	if exp < minBlockSizeExp || exp > maxBlockSizeExp {
		return 0, fmt.Errorf("%w: block size %d outside 64..8192", ErrBadHeader, size)
	}
	return exp, nil
}

// ParseComments decodes the comment header, the second packet of a Vorbis stream.
func ParseComments(data []byte) (vorbiscomment.Tags, error) {
	payload, err := headerPayload(data, packetComment)
	if err != nil {
		return vorbiscomment.Tags{}, err
	}
	// Vorbis requires the trailing framing bit; Opus and FLAC carry the same block without it.
	return vorbiscomment.Unmarshal(payload, true)
}

// AppendComments appends an encoded comment header to dst.
func AppendComments(dst []byte, t vorbiscomment.Tags) []byte {
	dst = append(dst, packetComment)
	dst = append(dst, headerSignature...)
	return t.AppendTo(dst, true)
}

// IsHeader reports whether a packet is one of the three header packets rather than audio.
//
// Vorbis I 4.2.1: the low bit of the type byte distinguishes them.
func IsHeader(data []byte) bool {
	return len(data) > 0 && data[0]&1 == 1
}
