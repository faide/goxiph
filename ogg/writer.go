package ogg

import (
	"fmt"
	"io"
)

// pageThreshold is the accumulated payload at which the muxer closes a page. libogg uses the same
// figure; it trades page overhead against how much data one corrupt page destroys.
const pageThreshold = 4096

// A Writer serializes pages onto a stream.
type Writer struct {
	w   io.Writer
	buf []byte
}

// NewWriter writes Ogg pages to w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w, buf: make([]byte, 0, MaxPageSize)}
}

// WritePage serializes p with a freshly computed checksum.
func (w *Writer) WritePage(p *Page) error {
	image, err := p.AppendTo(w.buf[:0])
	w.buf = image
	if err != nil {
		return err
	}
	_, err = w.w.Write(image)
	return err
}

// A Muxer packs packets into pages for one logical stream.
//
// Packets are buffered until pageThreshold is reached, so callers that need a packet to land on its
// own page — the Vorbis identification header, for instance — must call Flush.
type Muxer struct {
	w      *Writer
	serial uint32
	seq    uint32

	segs    []uint8
	payload []byte

	granule   int64
	hasEnd    bool // a packet ends on the page being built
	continued bool // the page being built opens with a continuation
	started   bool // a page has been built, so BOS is spent
	closed    bool

	// pending holds the last completed page, written only once the next one exists. The delay is
	// what lets Close put the end-of-stream flag on the page that carries the final packet, whether
	// or not the caller flushed after writing it. A codec that reads a stream terminated on a
	// separate empty page sees no final granule position and truncates the audio.
	pending *Page
}

// NewMuxer packs packets for the logical stream identified by serial.
func NewMuxer(w io.Writer, serial uint32) *Muxer {
	return &Muxer{
		w:       NewWriter(w),
		serial:  serial,
		segs:    make([]uint8, 0, MaxSegments),
		payload: make([]byte, 0, MaxPayload),
		granule: NoGranule,
	}
}

// WritePacket appends a packet, emitting pages as they fill.
//
// granulePos is the codec's position after this packet, or NoGranule when it has none. A packet
// larger than one page is split; the intervening pages correctly report no granule position.
func (m *Muxer) WritePacket(data []byte, granulePos int64) error {
	if m.closed {
		return fmt.Errorf("ogg: write to closed muxer for serial %#08x", m.serial)
	}

	// Close the previous page before adding to it rather than after. That leaves the most recent
	// packet buffered, so Close can put the end-of-stream flag and the final granule position on
	// the page that carries it.
	if len(m.payload) >= pageThreshold || len(m.segs) == MaxSegments {
		if err := m.emit(false); err != nil {
			return err
		}
	}

	rest := data
	for {
		space := MaxSegments - len(m.segs)
		if lacingLen(len(rest)) <= space {
			m.segs = appendLacing(m.segs, len(rest))
			m.payload = append(m.payload, rest...)
			m.granule = granulePos
			m.hasEnd = true
			return nil
		}
		// Fill the page with whole 255-byte segments and continue the packet on the next one.
		take := space * 255
		for range space {
			m.segs = append(m.segs, 255)
		}
		m.payload = append(m.payload, rest[:take]...)
		rest = rest[take:]
		if err := m.emit(true); err != nil {
			return err
		}
	}
}

// Flush closes the current page, forcing the next packet onto a new one. It is a no-op when nothing
// is buffered.
func (m *Muxer) Flush() error {
	if len(m.segs) == 0 {
		return nil
	}
	return m.emit(false)
}

// Close writes everything remaining and marks the end of the stream. Calling it twice is harmless.
func (m *Muxer) Close() error {
	if m.closed {
		return nil
	}
	m.closed = true

	if len(m.segs) > 0 {
		if err := m.emit(false); err != nil {
			return err
		}
	}
	if m.pending == nil {
		// Nothing was ever written, so terminate with a bare page.
		m.pending = &Page{HeaderType: FlagBOS, GranulePos: NoGranule, Serial: m.serial, Sequence: m.seq}
	}
	m.pending.HeaderType |= FlagEOS

	page := m.pending
	m.pending = nil
	return m.w.WritePage(page)
}

// emit completes the page under construction. openPacket reports that its final packet is
// unfinished, which suppresses the granule position and marks the next page as a continuation.
func (m *Muxer) emit(openPacket bool) error {
	var flags uint8
	if m.continued {
		flags |= FlagContinued
	}
	if !m.started {
		flags |= FlagBOS
		m.started = true
	}

	granule := NoGranule
	if m.hasEnd && !openPacket {
		granule = m.granule
	}

	// Segments and payload are copied because the page is held until the next one is built.
	page := &Page{
		HeaderType: flags,
		GranulePos: granule,
		Serial:     m.serial,
		Sequence:   m.seq,
		Segments:   append([]uint8(nil), m.segs...),
		Payload:    append([]byte(nil), m.payload...),
	}

	m.seq++
	m.segs = m.segs[:0]
	m.payload = m.payload[:0]
	m.hasEnd = false
	m.continued = openPacket
	m.granule = NoGranule

	prev := m.pending
	m.pending = page
	if prev == nil {
		return nil
	}
	return m.w.WritePage(prev)
}
