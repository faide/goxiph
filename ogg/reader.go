package ogg

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// ErrHole reports a gap in a logical stream, detected from a jump in page sequence numbers. The
// stream is still usable; the caller decides whether the missing audio matters.
var ErrHole = errors.New("ogg: hole in stream")

// readBuf must exceed MaxPageSize so a whole page is always peekable.
const readBuf = 1 << 17

// A Reader extracts pages from a byte stream, resynchronising past corruption.
type Reader struct {
	br      *bufio.Reader
	skipped int64
}

// NewReader reads Ogg pages from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, readBuf)}
}

// Skipped reports the total bytes discarded while resynchronising.
func (r *Reader) Skipped() int64 { return r.skipped }

// ReadPage returns the next valid page. Malformed or mis-checksummed data is skipped and counted in
// Skipped. The returned page aliases an internal buffer and is only valid until the next call; use
// Clone to retain it.
func (r *Reader) ReadPage() (*Page, error) {
	for {
		if err := r.sync(); err != nil {
			return nil, err
		}
		page, err := r.pageAtCursor()
		if err == nil {
			return page, nil
		}
		if !errors.Is(err, ErrBadPage) {
			return nil, err
		}
		// Capture pattern matched but the page did not hold up. Step over it and rescan; the
		// pattern can legitimately occur inside payload data.
		if _, derr := r.br.Discard(1); derr != nil {
			return nil, derr
		}
		r.skipped++
	}
}

// sync advances the cursor to the next capture pattern.
func (r *Reader) sync() error {
	for {
		buf, err := r.br.Peek(4)
		if len(buf) == 4 && string(buf) == magic {
			return nil
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(buf) < 4 {
				r.skipped += int64(len(buf))
				if _, derr := r.br.Discard(len(buf)); derr != nil {
					return derr
				}
				return io.EOF
			}
			return err
		}
		if _, err := r.br.Discard(1); err != nil {
			return err
		}
		r.skipped++
	}
}

// pageAtCursor decodes the page starting at the cursor, consuming it on success.
func (r *Reader) pageAtCursor() (*Page, error) {
	head, err := r.br.Peek(headerFixed)
	if err != nil {
		if len(head) < headerFixed {
			return nil, fmt.Errorf("%w: truncated header", ErrBadPage)
		}
		return nil, err
	}
	nsegs, _, err := parseHeader(head)
	if err != nil {
		return nil, err
	}

	table, err := r.br.Peek(headerFixed + nsegs)
	if err != nil && len(table) < headerFixed+nsegs {
		return nil, fmt.Errorf("%w: truncated segment table", ErrBadPage)
	}
	_, payloadLen, err := parseHeader(table)
	if err != nil {
		return nil, err
	}

	total := headerFixed + nsegs + payloadLen
	image, err := r.br.Peek(total)
	if err != nil && len(image) < total {
		return nil, fmt.Errorf("%w: truncated payload", ErrBadPage)
	}

	page, err := unmarshalPage(image)
	if err != nil {
		return nil, err
	}
	if _, err := r.br.Discard(total); err != nil {
		return nil, err
	}
	return page, nil
}

// streamState tracks packet reassembly for one logical stream.
type streamState struct {
	serial   uint32
	partial  []byte
	began    bool // partial holds the start of a packet
	packets  int  // packets emitted for this stream
	nextSeq  uint32
	seqValid bool
	ended    bool
}

// A Demuxer reassembles packets from pages, handling continuation across pages, multiplexed
// logical streams and chained streams.
type Demuxer struct {
	r       *Reader
	streams map[uint32]*streamState
	queue   []Packet
	hole    error
}

// NewDemuxer reads packets from r.
func NewDemuxer(r io.Reader) *Demuxer {
	return &Demuxer{r: NewReader(r), streams: make(map[uint32]*streamState)}
}

// Skipped reports the bytes discarded while resynchronising.
func (d *Demuxer) Skipped() int64 { return d.r.Skipped() }

// ReadPacket returns the next complete packet in stream order.
//
// A packet carries a granule position only when it is the last to end on its page; every other
// packet reports NoGranule. That is the framing's own rule, not a limitation here.
//
// When a page sequence gap is detected the packets already recovered are returned first, and a
// subsequent call reports ErrHole once before continuing.
func (d *Demuxer) ReadPacket() (Packet, error) {
	for {
		if len(d.queue) > 0 {
			p := d.queue[0]
			d.queue = d.queue[1:]
			return p, nil
		}
		if d.hole != nil {
			err := d.hole
			d.hole = nil
			return Packet{}, err
		}
		page, err := d.r.ReadPage()
		if err != nil {
			return Packet{}, err
		}
		d.consume(page)
	}
}

// consume splits a page into packets, appending completed ones to the queue.
func (d *Demuxer) consume(page *Page) {
	st := d.streams[page.Serial]
	if st == nil || (page.BOS() && st.ended) {
		// A BOS for a serial we have already closed is a chained stream, so start fresh.
		st = &streamState{serial: page.Serial}
		d.streams[page.Serial] = st
	}

	if st.seqValid && page.Sequence != st.nextSeq {
		d.hole = fmt.Errorf("%w: serial %#08x expected page %d, got %d",
			ErrHole, page.Serial, st.nextSeq, page.Sequence)
		st.partial = st.partial[:0]
		st.began = false
	}
	st.nextSeq = page.Sequence + 1
	st.seqValid = true

	if page.Continued() {
		if !st.began {
			// Continuation with nothing to continue: the opening pages were lost. Drop the
			// fragment rather than emitting a truncated packet.
			d.dropLeadingFragment(page)
			return
		}
	} else if st.began {
		// A fresh packet starts here even though one was still open, so the tail was lost.
		st.partial = st.partial[:0]
		st.began = false
	}

	lastEnd := lastTerminator(page.Segments)
	off := 0
	for i, seg := range page.Segments {
		st.partial = append(st.partial, page.Payload[off:off+int(seg)]...)
		st.began = true
		off += int(seg)
		if seg == 255 {
			continue
		}
		pkt := Packet{
			Data:       append([]byte(nil), st.partial...),
			GranulePos: NoGranule,
			Serial:     page.Serial,
			FirstPage:  page.BOS() && st.packets == 0,
			LastPage:   page.EOS() && i == lastEnd,
		}
		if i == lastEnd {
			pkt.GranulePos = page.GranulePos
		}
		d.queue = append(d.queue, pkt)
		st.packets++
		st.partial = st.partial[:0]
		st.began = false
	}
	if page.EOS() {
		st.ended = true
	}
}

// dropLeadingFragment skips the continuation fragment at the start of a page and consumes the rest
// normally.
func (d *Demuxer) dropLeadingFragment(page *Page) {
	st := d.streams[page.Serial]
	off := 0
	for i, seg := range page.Segments {
		off += int(seg)
		if seg < 255 {
			trimmed := &Page{
				HeaderType: page.HeaderType &^ FlagContinued,
				GranulePos: page.GranulePos,
				Serial:     page.Serial,
				Sequence:   page.Sequence,
				Segments:   page.Segments[i+1:],
				Payload:    page.Payload[off:],
			}
			st.seqValid = true
			st.nextSeq = page.Sequence + 1
			d.consume(trimmed)
			return
		}
	}
	// Whole page is one continuing fragment with no terminator; nothing to recover.
}

// lastTerminator is the index of the final segment that ends a packet, or -1 if none does.
func lastTerminator(segs []uint8) int {
	for i := len(segs) - 1; i >= 0; i-- {
		if segs[i] < 255 {
			return i
		}
	}
	return -1
}
