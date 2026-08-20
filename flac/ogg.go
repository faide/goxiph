package flac

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/faide/goxiph/ogg"
	"github.com/faide/goxiph/vorbiscomment"
)

// oggSignature opens an Ogg physical stream, and so an Ogg-encapsulated FLAC file.
const oggSignature = "OggS"

// oggMappingMagic opens the first packet of a FLAC-in-Ogg logical stream. RFC 9639 section 10.1.
var oggMappingMagic = []byte{0x7F, 'F', 'L', 'A', 'C'}

// oggMappingHeaderLen is the fixed size of that first packet: the magic, the mapping version, the
// header packet count, the native signature, and a stream info block with its own header.
const oggMappingHeaderLen = 5 + 2 + 2 + 4 + 4 + 34

// newOggDecoder reads the FLAC-in-Ogg header packets, leaving the demuxer at the first audio packet.
func newOggDecoder(br *bufio.Reader) (*Decoder, error) {
	dem := ogg.NewDemuxer(br)

	first, err := dem.ReadPacket()
	if err != nil {
		return nil, fmt.Errorf("%w: reading the mapping header: %w", ErrBadStream, err)
	}
	meta, err := parseOggMappingHeader(first.Data)
	if err != nil {
		return nil, err
	}
	serial := first.Serial
	declared := oggHeaderPacketCount(first.Data)

	// The remaining header packets carry one metadata block each. The count field in the mapping
	// header may be zero for "unknown", so the last-block flag is what terminates the run; the
	// count is cross-checked afterwards when it was given.
	seen := 0
	for {
		p, err := dem.ReadPacket()
		if err != nil {
			return nil, fmt.Errorf("%w: reading a metadata packet: %w", ErrBadStream, err)
		}
		if p.Serial != serial {
			continue // another logical stream in a multiplexed file
		}
		if len(p.Data) < 4 {
			return nil, fmt.Errorf("%w: metadata packet of %d bytes", ErrBadStream, len(p.Data))
		}

		last := p.Data[0]&0x80 != 0
		kind := int(p.Data[0] & 0x7f)
		size := int(p.Data[1])<<16 | int(p.Data[2])<<8 | int(p.Data[3])
		if kind == blockForbidden {
			return nil, fmt.Errorf("%w: metadata block type 127 is forbidden", ErrBadStream)
		}
		if 4+size > len(p.Data) {
			return nil, fmt.Errorf("%w: metadata block declares %d bytes, packet holds %d",
				ErrBadStream, size, len(p.Data)-4)
		}

		if kind == blockVorbisComment {
			// As in native FLAC, the block carries no trailing framing bit.
			if meta.Tags, err = vorbiscomment.Unmarshal(p.Data[4:4+size], false); err != nil {
				return nil, err
			}
		}
		seen++
		if last {
			break
		}
	}
	if declared != 0 && seen != declared {
		return nil, fmt.Errorf("%w: mapping header declares %d metadata packets, found %d",
			ErrBadStream, declared, seen)
	}

	d := newDecoder(br, dem, meta)
	d.serial = serial
	return d, nil
}

// parseOggMappingHeader decodes the first packet of a FLAC-in-Ogg stream.
func parseOggMappingHeader(p []byte) (Metadata, error) {
	var m Metadata
	if len(p) < oggMappingHeaderLen {
		return m, fmt.Errorf("%w: mapping header is %d bytes, want %d",
			ErrBadStream, len(p), oggMappingHeaderLen)
	}
	if string(p[:5]) != string(oggMappingMagic) {
		return m, fmt.Errorf("%w: mapping header starts %#x, want %#x", ErrBadStream, p[:5], oggMappingMagic)
	}
	if p[5] != 1 {
		return m, fmt.Errorf("%w: FLAC-in-Ogg mapping version %d.%d is unsupported", ErrBadStream, p[5], p[6])
	}
	if string(p[9:13]) != Signature {
		return m, fmt.Errorf("%w: mapping header lacks the %q signature", ErrBadStream, Signature)
	}

	// Bytes 13..16 are the stream info block's own metadata header; its type must match.
	if kind := int(p[13] & 0x7f); kind != blockStreaminfo {
		return m, fmt.Errorf("%w: first metadata block is type %d, want stream info", ErrBadStream, kind)
	}

	info, err := parseStreamInfo(p[17 : 17+34])
	if err != nil {
		return m, err
	}
	m.StreamInfo = info
	return m, nil
}

// oggHeaderPacketCount reports the metadata packet count the mapping header declares, or zero when
// the encoder left it unknown.
func oggHeaderPacketCount(p []byte) int {
	if len(p) < 9 {
		return 0
	}
	return int(binary.BigEndian.Uint16(p[7:9]))
}

// nextOggFrame returns the next audio packet, which carries exactly one FLAC frame.
//
// Ogg delimits the frames, so unlike the native path there is no scanning for sync codes and a sync
// pattern inside residual data cannot mislead the reader.
func (d *Decoder) nextOggFrame() ([]byte, error) {
	for {
		p, err := d.dem.ReadPacket()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.EOF
			}
			return nil, err
		}
		if p.Serial != d.serial {
			continue
		}
		if len(p.Data) == 0 {
			continue
		}
		return p.Data, nil
	}
}
