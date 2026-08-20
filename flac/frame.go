package flac

import (
	"errors"
	"fmt"

	"github.com/faide/goxiph/internal/bitio"
)

// ErrBadFrame reports a frame that violates RFC 9639 section 9.
var ErrBadFrame = errors.New("flac: malformed frame")

// frameSync is the 15-bit code opening every frame, followed by the blocking strategy bit.
const frameSync = 0b111111111111100

// Stereo decorrelation modes. RFC 9639 section 9.1.3.
const (
	channelsIndependent = iota
	channelsLeftSide
	channelsSideRight
	channelsMidSide
)

// blockSizeTable maps the block size bits. Zero marks a code that carries the size elsewhere or is
// reserved. RFC 9639 section 9.1.1.
var blockSizeTable = [16]int{
	0, 192, 576, 1152, 2304, 4608, 0, 0,
	256, 512, 1024, 2048, 4096, 8192, 16384, 32768,
}

// sampleRateTable maps the sample rate bits. Negative marks a code that carries the rate elsewhere.
// RFC 9639 section 9.1.2.
var sampleRateTable = [16]int{
	0, 88200, 176400, 192000, 8000, 16000, 22050, 24000,
	32000, 44100, 48000, 96000, -1, -1, -1, -2,
}

// bitDepthTable maps the bit depth bits. Zero means the depth comes from the stream info block, and
// -1 marks the reserved code. RFC 9639 section 9.1.4.
var bitDepthTable = [8]int{0, 8, 12, -1, 16, 20, 24, 32}

// frameHeader is one decoded frame header.
type frameHeader struct {
	variableBlocking bool
	blockSize        int
	sampleRate       int
	channels         int
	channelMode      int
	bitsPerSample    int
	codedNumber      uint64
	headerBytes      int // header length, for the checksum
}

// sideChannel reports which subframe index carries the side channel, or -1 when none does.
//
// The side channel needs one extra bit of depth because a difference can be twice the magnitude the
// declared depth allows.
func (h frameHeader) sideChannel() int {
	switch h.channelMode {
	case channelsLeftSide:
		return 1
	case channelsSideRight:
		return 0
	case channelsMidSide:
		return 1
	default:
		return -1
	}
}

// readFrameHeader decodes a frame header and verifies its checksum. RFC 9639 section 9.1.
func readFrameHeader(r *bitio.MSBReader, info StreamInfo, raw []byte) (frameHeader, error) {
	var h frameHeader
	start := r.BitsRead() / 8

	sync, err := r.Read(15)
	if err != nil {
		return h, fmt.Errorf("%w: truncated at sync code", ErrBadFrame)
	}
	if sync != frameSync {
		return h, fmt.Errorf("%w: sync %#04x, want %#04x", ErrBadFrame, sync, frameSync)
	}
	if h.variableBlocking, err = r.ReadBool(); err != nil {
		return h, fmt.Errorf("%w: truncated at blocking strategy", ErrBadFrame)
	}

	blockBits, err := r.Read(4)
	if err != nil {
		return h, fmt.Errorf("%w: truncated at block size bits", ErrBadFrame)
	}
	rateBits, err := r.Read(4)
	if err != nil {
		return h, fmt.Errorf("%w: truncated at sample rate bits", ErrBadFrame)
	}
	chanBits, err := r.Read(4)
	if err != nil {
		return h, fmt.Errorf("%w: truncated at channel bits", ErrBadFrame)
	}
	depthBits, err := r.Read(3)
	if err != nil {
		return h, fmt.Errorf("%w: truncated at bit depth bits", ErrBadFrame)
	}
	reserved, err := r.Read(1)
	if err != nil {
		return h, fmt.Errorf("%w: truncated at reserved bit", ErrBadFrame)
	}
	if reserved != 0 {
		return h, fmt.Errorf("%w: reserved bit set", ErrBadFrame)
	}

	if h.codedNumber, err = readCodedNumber(r); err != nil {
		return h, err
	}

	// Uncommon block size and sample rate follow the coded number, in that order.
	switch blockBits {
	case 0:
		return h, fmt.Errorf("%w: block size code 0 is reserved", ErrBadFrame)
	case 6:
		v, err := r.Read(8)
		if err != nil {
			return h, fmt.Errorf("%w: truncated at 8-bit block size", ErrBadFrame)
		}
		h.blockSize = int(v) + 1
	case 7:
		v, err := r.Read(16)
		if err != nil {
			return h, fmt.Errorf("%w: truncated at 16-bit block size", ErrBadFrame)
		}
		if v == 65535 {
			return h, fmt.Errorf("%w: block size 65536 cannot appear in stream info", ErrBadFrame)
		}
		h.blockSize = int(v) + 1
	default:
		h.blockSize = blockSizeTable[blockBits]
	}

	switch sampleRateTable[rateBits] {
	case -2:
		return h, fmt.Errorf("%w: sample rate code 15 is forbidden", ErrBadFrame)
	case -1:
		switch rateBits {
		case 12:
			v, err := r.Read(8)
			if err != nil {
				return h, fmt.Errorf("%w: truncated at 8-bit sample rate", ErrBadFrame)
			}
			h.sampleRate = int(v) * 1000
		case 13:
			v, err := r.Read(16)
			if err != nil {
				return h, fmt.Errorf("%w: truncated at 16-bit sample rate", ErrBadFrame)
			}
			h.sampleRate = int(v)
		default:
			v, err := r.Read(16)
			if err != nil {
				return h, fmt.Errorf("%w: truncated at 16-bit sample rate", ErrBadFrame)
			}
			h.sampleRate = int(v) * 10
		}
	case 0:
		h.sampleRate = info.SampleRate
	default:
		h.sampleRate = sampleRateTable[rateBits]
	}

	if chanBits >= 11 {
		return h, fmt.Errorf("%w: channel code %d is reserved", ErrBadFrame, chanBits)
	}
	if chanBits >= 8 {
		h.channels = 2
		h.channelMode = int(chanBits) - 7
	} else {
		h.channels = int(chanBits) + 1
		h.channelMode = channelsIndependent
	}

	switch d := bitDepthTable[depthBits]; d {
	case -1:
		return h, fmt.Errorf("%w: bit depth code 3 is reserved", ErrBadFrame)
	case 0:
		h.bitsPerSample = info.BitsPerSample
	default:
		h.bitsPerSample = d
	}
	if h.bitsPerSample < 4 || h.bitsPerSample > 32 {
		return h, fmt.Errorf("%w: %d bits per sample", ErrBadFrame, h.bitsPerSample)
	}
	if h.blockSize <= 0 {
		return h, fmt.Errorf("%w: block size %d", ErrBadFrame, h.blockSize)
	}

	if !r.ByteAligned() {
		return h, fmt.Errorf("%w: header did not end on a byte boundary", ErrBadFrame)
	}
	h.headerBytes = r.BitsRead()/8 - start

	stored, err := r.Read(8)
	if err != nil {
		return h, fmt.Errorf("%w: truncated at header checksum", ErrBadFrame)
	}
	if got := crc8(raw[start : start+h.headerBytes]); got != uint8(stored) {
		return h, fmt.Errorf("%w: header checksum %#02x, computed %#02x", ErrBadFrame, stored, got)
	}
	return h, nil
}

// readCodedNumber decodes the frame or sample number. RFC 9639 section 9.1.5.
//
// The encoding follows UTF-8's shape but runs to 36 bits over seven octets, so a standard UTF-8
// decoder rejects the longer forms.
func readCodedNumber(r *bitio.MSBReader) (uint64, error) {
	first, err := r.Read(8)
	if err != nil {
		return 0, fmt.Errorf("%w: truncated at coded number", ErrBadFrame)
	}

	if first < 0x80 {
		return first, nil
	}

	// Count the leading ones to find the length, then take what remains of the first octet.
	var extra int
	var value uint64
	switch {
	case first&0xE0 == 0xC0:
		extra, value = 1, first&0x1F
	case first&0xF0 == 0xE0:
		extra, value = 2, first&0x0F
	case first&0xF8 == 0xF0:
		extra, value = 3, first&0x07
	case first&0xFC == 0xF8:
		extra, value = 4, first&0x03
	case first&0xFE == 0xFC:
		extra, value = 5, first&0x01
	case first == 0xFE:
		extra, value = 6, 0
	default:
		return 0, fmt.Errorf("%w: coded number starts %#02x", ErrBadFrame, first)
	}

	for i := range extra {
		b, err := r.Read(8)
		if err != nil {
			return 0, fmt.Errorf("%w: truncated at coded number octet %d", ErrBadFrame, i+1)
		}
		if b&0xC0 != 0x80 {
			return 0, fmt.Errorf("%w: coded number continuation octet %#02x", ErrBadFrame, b)
		}
		value = value<<6 | (b & 0x3F)
	}
	return value, nil
}
