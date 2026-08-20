package flac

import (
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/faide/goxiph/internal/bitio"
	"github.com/faide/goxiph/vorbiscomment"
)

// Block is one decoded frame: planar integer samples, one slice per channel.
//
// Samples are sign-extended into int32 and carry the stream's declared bit depth.
type Block struct {
	Samples       [][]int32
	SampleRate    int
	BitsPerSample int
}

// Frames reports the number of samples per channel.
func (b *Block) Frames() int {
	if len(b.Samples) == 0 {
		return 0
	}
	return len(b.Samples[0])
}

// Decoder reads a native FLAC stream.
//
// A Decoder is not safe for concurrent use.
type Decoder struct {
	br   *bufio.Reader
	meta Metadata

	subframes [][]int32
	raw       []byte
	done      bool
}

// NewDecoder reads the stream signature and metadata, leaving the reader at the first frame.
func NewDecoder(r io.Reader) (*Decoder, error) {
	br := bufio.NewReaderSize(r, 1<<16)
	meta, err := readMetadata(br)
	if err != nil {
		return nil, err
	}

	d := &Decoder{br: br, meta: meta}
	d.subframes = make([][]int32, meta.StreamInfo.Channels)
	for i := range d.subframes {
		d.subframes[i] = make([]int32, meta.StreamInfo.MaxBlockSize)
	}
	return d, nil
}

// StreamInfo returns the mandatory metadata block.
func (d *Decoder) StreamInfo() StreamInfo { return d.meta.StreamInfo }

// Comments returns the Vorbis comment metadata, empty when the stream carries none.
func (d *Decoder) Comments() vorbiscomment.Tags { return d.meta.Tags }

// Next decodes the next frame. It returns io.EOF at the end of the stream.
//
// The returned Block aliases the decoder's buffers and stays valid only until the next call.
func (d *Decoder) Next() (*Block, error) {
	if d.done {
		return nil, io.EOF
	}

	frame, err := d.readFrameBytes()
	if err != nil {
		if errors.Is(err, io.EOF) {
			d.done = true
		}
		return nil, err
	}

	r := bitio.NewMSBReader(frame)
	h, err := readFrameHeader(r, d.meta.StreamInfo, frame)
	if err != nil {
		return nil, err
	}
	if h.channels > len(d.subframes) {
		return nil, fmt.Errorf("%w: frame has %d channels, stream info declares %d",
			ErrBadFrame, h.channels, len(d.subframes))
	}
	if h.blockSize > d.meta.StreamInfo.MaxBlockSize {
		return nil, fmt.Errorf("%w: block size %d exceeds the declared maximum %d",
			ErrBadFrame, h.blockSize, d.meta.StreamInfo.MaxBlockSize)
	}

	side := h.sideChannel()
	for ch := range h.channels {
		depth := uint(h.bitsPerSample)
		if ch == side {
			depth++
		}
		if err := readSubframe(r, d.subframes[ch][:h.blockSize], depth); err != nil {
			return nil, fmt.Errorf("channel %d: %w", ch, err)
		}
	}

	r.AlignByte()
	stored, err := r.Read(16)
	if err != nil {
		return nil, fmt.Errorf("%w: truncated at frame checksum", ErrBadFrame)
	}
	covered := r.BitsRead()/8 - 2
	if got := crc16(frame[:covered]); got != uint16(stored) {
		return nil, fmt.Errorf("%w: frame checksum %#04x, computed %#04x", ErrBadFrame, stored, got)
	}

	undecorrelate(d.subframes, h.blockSize, h.channelMode)

	block := &Block{
		Samples:       make([][]int32, h.channels),
		SampleRate:    h.sampleRate,
		BitsPerSample: h.bitsPerSample,
	}
	for ch := range h.channels {
		block.Samples[ch] = d.subframes[ch][:h.blockSize]
	}
	return block, nil
}

// undecorrelate restores left and right from the stored channel pair. RFC 9639 section 4.2.
func undecorrelate(ch [][]int32, n, mode int) {
	switch mode {
	case channelsLeftSide:
		// Stored: left and (left - right).
		for i := range n {
			ch[1][i] = ch[0][i] - ch[1][i]
		}
	case channelsSideRight:
		// Stored: (left - right) and right.
		for i := range n {
			ch[0][i] += ch[1][i]
		}
	case channelsMidSide:
		// The mid channel dropped its low bit on encode; the side channel's parity restores it,
		// which is why the right shift is not lossy.
		for i := range n {
			mid, sideV := int64(ch[0][i]), int64(ch[1][i])
			mid = mid<<1 | (sideV & 1)
			ch[0][i] = int32((mid + sideV) >> 1)
			ch[1][i] = int32((mid - sideV) >> 1)
		}
	}
}

// readFrameBytes reads one complete frame by scanning for the next sync code.
//
// A frame carries no length, so its end is found by locating the following frame's sync code. The
// frame checksum then confirms the split, which is what makes a sync pattern occurring inside
// residual data recoverable rather than fatal.
func (d *Decoder) readFrameBytes() ([]byte, error) {
	// Peek far enough to find the next sync code after this frame.
	const window = 1 << 16

	buf, err := d.br.Peek(window)
	if len(buf) < 2 {
		if err != nil {
			return nil, io.EOF
		}
		return nil, io.EOF
	}
	if buf[0] != 0xFF || buf[1]&0xFC != 0xF8 {
		return nil, fmt.Errorf("%w: no frame sync at the cursor", ErrBadFrame)
	}

	for end := 2; end+1 < len(buf); end++ {
		if buf[end] != 0xFF || buf[end+1]&0xFC != 0xF8 {
			continue
		}
		if d.frameChecksumHolds(buf[:end]) {
			d.raw = append(d.raw[:0], buf[:end]...)
			if _, err := d.br.Discard(end); err != nil {
				return nil, err
			}
			return d.raw, nil
		}
	}

	// No later sync code held up, so this is the final frame and runs to the end of the stream.
	if err == nil {
		return nil, fmt.Errorf("%w: frame exceeds the %d byte window", ErrBadFrame, window)
	}
	d.raw = append(d.raw[:0], buf...)
	if _, derr := d.br.Discard(len(buf)); derr != nil {
		return nil, derr
	}
	d.done = true
	return d.raw, nil
}

// frameChecksumHolds reports whether p ends on a valid frame checksum.
func (d *Decoder) frameChecksumHolds(p []byte) bool {
	if len(p) < 6 {
		return false
	}
	stored := uint16(p[len(p)-2])<<8 | uint16(p[len(p)-1])
	return crc16(p[:len(p)-2]) == stored
}

// DecodeAll reads a whole FLAC stream into one set of planar channels.
func DecodeAll(r io.Reader) ([][]int32, StreamInfo, error) {
	d, err := NewDecoder(r)
	if err != nil {
		return nil, StreamInfo{}, err
	}
	info := d.StreamInfo()

	out := make([][]int32, info.Channels)
	for {
		block, err := d.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, info, err
		}
		for ch := range block.Samples {
			if ch >= len(out) {
				break
			}
			out[ch] = append(out[ch], block.Samples[ch]...)
		}
	}
	return out, info, nil
}
