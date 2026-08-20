package ogg

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Page header flags, RFC 3533 section 6.
const (
	FlagContinued uint8 = 0x01 // first packet on this page continues from the previous page
	FlagBOS       uint8 = 0x02 // beginning of logical stream
	FlagEOS       uint8 = 0x04 // end of logical stream
)

const (
	magic = "OggS"

	// headerFixed is the part of the page header preceding the segment table.
	headerFixed = 27

	// MaxSegments is the largest segment table a page can carry, bounded by the one-byte count.
	MaxSegments = 255

	// MaxPayload is the most data one page can hold: 255 segments of at most 255 bytes.
	MaxPayload = MaxSegments * 255

	// MaxPageSize bounds a complete page image.
	MaxPageSize = headerFixed + MaxSegments + MaxPayload
)

// Byte offsets within the fixed header.
const (
	offVersion    = 4
	offHeaderType = 5
	offGranule    = 6
	offSerial     = 14
	offSequence   = 18
	offCRC        = 22
	offSegments   = 26
)

var (
	// ErrBadPage reports a page that is malformed or fails its checksum. A reader resynchronises
	// past it rather than giving up.
	ErrBadPage = errors.New("ogg: malformed page")

	// ErrNoGranule is the granule position of a page on which no packet ends.
	ErrNoGranule = errors.New("ogg: page carries no granule position")
)

// NoGranule marks a page on which no packet ends. RFC 3533 requires -1, not 0.
const NoGranule int64 = -1

// A Page is one Ogg framing unit.
//
// Segments is kept alongside Payload because a given payload can be laced in more than one valid
// way; preserving it is what makes parse followed by Marshal byte-identical.
type Page struct {
	HeaderType uint8
	GranulePos int64
	Serial     uint32
	Sequence   uint32
	Segments   []uint8
	Payload    []byte
}

// Continued reports whether the first packet on the page continues from the previous page.
func (p *Page) Continued() bool { return p.HeaderType&FlagContinued != 0 }

// BOS reports whether the page begins a logical stream.
func (p *Page) BOS() bool { return p.HeaderType&FlagBOS != 0 }

// EOS reports whether the page ends a logical stream.
func (p *Page) EOS() bool { return p.HeaderType&FlagEOS != 0 }

// HeaderLen is the size of the page header including the segment table.
func (p *Page) HeaderLen() int { return headerFixed + len(p.Segments) }

// Len is the size of the complete page image.
func (p *Page) Len() int { return p.HeaderLen() + len(p.Payload) }

// EndsPacket reports whether at least one packet finishes on this page, which is the condition for
// the granule position to be meaningful.
func (p *Page) EndsPacket() bool {
	for _, s := range p.Segments {
		if s < 255 {
			return true
		}
	}
	return false
}

// validate checks the invariants Marshal relies on.
func (p *Page) validate() error {
	if len(p.Segments) > MaxSegments {
		return fmt.Errorf("%w: %d segments, max %d", ErrBadPage, len(p.Segments), MaxSegments)
	}
	total := 0
	for _, s := range p.Segments {
		total += int(s)
	}
	if total != len(p.Payload) {
		return fmt.Errorf("%w: segment table sums to %d, payload is %d bytes", ErrBadPage, total, len(p.Payload))
	}
	return nil
}

// AppendTo appends the serialized page, with a freshly computed checksum, to dst.
func (p *Page) AppendTo(dst []byte) ([]byte, error) {
	if err := p.validate(); err != nil {
		return dst, err
	}

	start := len(dst)
	dst = append(dst, magic...)
	dst = append(dst, 0, p.HeaderType)
	dst = binary.LittleEndian.AppendUint64(dst, uint64(p.GranulePos))
	dst = binary.LittleEndian.AppendUint32(dst, p.Serial)
	dst = binary.LittleEndian.AppendUint32(dst, p.Sequence)
	dst = binary.LittleEndian.AppendUint32(dst, 0) // checksum, filled in below
	dst = append(dst, uint8(len(p.Segments)))
	dst = append(dst, p.Segments...)
	dst = append(dst, p.Payload...)

	image := dst[start:]
	binary.LittleEndian.PutUint32(image[offCRC:], crcOf(image))
	return dst, nil
}

// Marshal returns the serialized page image.
func (p *Page) Marshal() ([]byte, error) {
	return p.AppendTo(make([]byte, 0, p.Len()))
}

// parseHeader reads the fixed header and segment table from buf, returning the segment count and
// the payload length. buf must hold at least headerFixed bytes.
func parseHeader(buf []byte) (nsegs, payloadLen int, err error) {
	if string(buf[:4]) != magic {
		return 0, 0, fmt.Errorf("%w: bad capture pattern", ErrBadPage)
	}
	if buf[offVersion] != 0 {
		return 0, 0, fmt.Errorf("%w: version %d", ErrBadPage, buf[offVersion])
	}
	nsegs = int(buf[offSegments])
	if len(buf) < headerFixed+nsegs {
		return nsegs, 0, nil // caller must supply more bytes
	}
	for _, s := range buf[headerFixed : headerFixed+nsegs] {
		payloadLen += int(s)
	}
	return nsegs, payloadLen, nil
}

// unmarshalPage decodes a complete page image and verifies its checksum. Payload and Segments
// alias image; callers that retain the page must copy it.
func unmarshalPage(image []byte) (*Page, error) {
	if len(image) < headerFixed {
		return nil, fmt.Errorf("%w: %d bytes, need at least %d", ErrBadPage, len(image), headerFixed)
	}
	nsegs, payloadLen, err := parseHeader(image)
	if err != nil {
		return nil, err
	}
	want := headerFixed + nsegs + payloadLen
	if len(image) != want {
		return nil, fmt.Errorf("%w: image is %d bytes, header describes %d", ErrBadPage, len(image), want)
	}

	stored := binary.LittleEndian.Uint32(image[offCRC:])
	binary.LittleEndian.PutUint32(image[offCRC:], 0)
	computed := crcOf(image)
	binary.LittleEndian.PutUint32(image[offCRC:], stored)
	if computed != stored {
		return nil, fmt.Errorf("%w: checksum %#08x, computed %#08x", ErrBadPage, stored, computed)
	}

	return &Page{
		HeaderType: image[offHeaderType],
		GranulePos: int64(binary.LittleEndian.Uint64(image[offGranule:])),
		Serial:     binary.LittleEndian.Uint32(image[offSerial:]),
		Sequence:   binary.LittleEndian.Uint32(image[offSequence:]),
		Segments:   image[headerFixed : headerFixed+nsegs],
		Payload:    image[headerFixed+nsegs:],
	}, nil
}

// Clone returns a page that shares no storage with p.
func (p *Page) Clone() *Page {
	c := *p
	c.Segments = append([]uint8(nil), p.Segments...)
	c.Payload = append([]byte(nil), p.Payload...)
	return &c
}

// lacingLen is the number of segment-table entries a packet of n bytes needs.
//
// The trailing entry is n%255, so a packet whose length is a multiple of 255 gets an extra zero
// terminator; without it the reader would treat the packet as continuing onto the next page.
func lacingLen(n int) int { return n/255 + 1 }

// appendLacing appends the segment table entries describing a packet of n bytes.
func appendLacing(dst []uint8, n int) []uint8 {
	for ; n >= 255; n -= 255 {
		dst = append(dst, 255)
	}
	return append(dst, uint8(n))
}
