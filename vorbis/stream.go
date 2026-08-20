package vorbis

import (
	"errors"
	"fmt"
	"io"

	"github.com/faide/goxiph/audio"
	"github.com/faide/goxiph/ogg"
	"github.com/faide/goxiph/vorbiscomment"
)

// ErrBadStream reports a stream whose packets do not form a Vorbis logical stream.
var ErrBadStream = errors.New("vorbis: malformed stream")

// StreamDecoder decodes a complete Ogg Vorbis logical stream, handling the container and the
// granule positions so callers get exactly the samples the encoder intended.
//
// The raw Decoder emits every lapped sample, which overruns the true stream length: the first block
// only primes the overlap, and the encoder signals the real endpoints through granule positions
// rather than through the block boundaries.
type StreamDecoder struct {
	dem  *ogg.Demuxer
	dec  *Decoder
	tags vorbiscomment.Tags

	scratch *audio.Buffer
	serial  uint32

	decoded    int64 // samples produced before trimming
	frontTrim  int64 // samples still to drop from the head
	seenFirst  bool
	finished   bool
	haveSerial bool
}

// NewStreamDecoder reads the three header packets from r and returns a decoder ready for audio.
func NewStreamDecoder(r io.Reader) (*StreamDecoder, error) {
	s := &StreamDecoder{dem: ogg.NewDemuxer(r), dec: NewDecoder()}

	for headers := 0; headers < 3; {
		p, err := s.dem.ReadPacket()
		if err != nil {
			return nil, fmt.Errorf("%w: reading header %d: %w", ErrBadStream, headers+1, err)
		}
		if !s.haveSerial {
			s.serial, s.haveSerial = p.Serial, true
		} else if p.Serial != s.serial {
			continue // another logical stream in a multiplexed file
		}

		switch headers {
		case 0:
			info, err := ParseInfo(p.Data)
			if err != nil {
				return nil, err
			}
			s.dec.SetInfo(info)
			if s.scratch, err = audio.NewBuffer(info.Format(), 0); err != nil {
				return nil, err
			}
		case 1:
			tags, err := ParseComments(p.Data)
			if err != nil {
				return nil, err
			}
			s.tags = tags
		case 2:
			setup, err := ParseSetup(p.Data, s.dec.Info())
			if err != nil {
				return nil, err
			}
			if err := s.dec.SetSetup(setup); err != nil {
				return nil, err
			}
		}
		headers++
	}
	return s, nil
}

// Format returns the PCM format of the stream.
func (s *StreamDecoder) Format() audio.Format { return s.dec.Format() }

// Info returns the identification header.
func (s *StreamDecoder) Info() Info { return s.dec.Info() }

// Comments returns the metadata from the comment header.
func (s *StreamDecoder) Comments() vorbiscomment.Tags { return s.tags }

// Next decodes one packet, replacing out's contents with the samples it yields.
//
// A packet can legitimately yield zero samples: the first one only primes the overlap. Next returns
// io.EOF once the stream has ended.
func (s *StreamDecoder) Next(out *audio.Buffer) error {
	if s.finished {
		return io.EOF
	}

	for {
		p, err := s.dem.ReadPacket()
		if errors.Is(err, io.EOF) {
			s.finished = true
			out.Resize(0)
			return io.EOF
		}
		if err != nil {
			return err
		}
		if p.Serial != s.serial {
			continue
		}
		if IsHeader(p.Data) {
			continue // a chained stream's headers; this decoder follows one stream
		}

		s.scratch.Resize(0)
		if err := s.dec.DecodePacket(p.Data, s.scratch); err != nil {
			return err
		}
		produced := int64(s.scratch.Frames())
		s.decoded += produced

		start, end := int64(0), produced
		start, end = s.applyTrim(p, start, end)

		if end <= start {
			out.Resize(0)
			if p.LastPage {
				s.finished = true
			}
			if s.finished {
				return io.EOF
			}
			continue
		}

		n := int(end - start)
		out.Resize(n)
		for ch := range out.Data {
			copy(out.Data[ch], s.scratch.Data[ch][start:end])
		}
		if p.LastPage {
			s.finished = true
		}
		return nil
	}
}

// applyTrim narrows the produced range according to the packet's granule position.
//
// Vorbis I A.2: the first audio page's granule position accounts only for the samples that survive
// lapping, and the final page's may fall below the arithmetic total in order to truncate. Those two
// rules are how a stream gets a length that is not a multiple of the block size.
func (s *StreamDecoder) applyTrim(p ogg.Packet, start, end int64) (int64, int64) {
	if s.frontTrim > 0 {
		drop := min(s.frontTrim, end-start)
		start += drop
		s.frontTrim -= drop
	}

	if p.GranulePos == ogg.NoGranule {
		return start, end
	}
	excess := s.decoded - p.GranulePos

	// The final page's granule position defines the stream's true length, so any surplus is
	// trailing overlap. This is checked before the first-page rule because a stream short enough to
	// fit on one page is both the first page and the last, and trimming its surplus from the head
	// would keep the right number of samples while emitting the wrong ones.
	if p.LastPage {
		s.seenFirst = true
		if excess > 0 {
			end -= min(excess, end-start)
			s.decoded = p.GranulePos
		}
		return start, end
	}

	if !s.seenFirst {
		s.seenFirst = true
		// Samples ahead of the first granule position were never part of the stream. No libvorbis
		// output in the corpus reaches this branch, so it is covered by unit tests alone.
		if excess > 0 {
			drop := min(excess, end-start)
			start += drop
			s.frontTrim = excess - drop
			s.decoded = p.GranulePos
		}
	}
	return start, end
}

// DecodeAll reads a whole Ogg Vorbis stream into one buffer.
func DecodeAll(r io.Reader) (*audio.Buffer, vorbiscomment.Tags, error) {
	s, err := NewStreamDecoder(r)
	if err != nil {
		return nil, vorbiscomment.Tags{}, err
	}

	out, err := audio.NewBuffer(s.Format(), 0)
	if err != nil {
		return nil, s.Comments(), err
	}
	chunk, err := audio.NewBuffer(s.Format(), 0)
	if err != nil {
		return nil, s.Comments(), err
	}

	for {
		err := s.Next(chunk)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, s.Comments(), err
		}
		n := chunk.Frames()
		if n == 0 {
			continue
		}
		base := out.Frames()
		out.Resize(base + n)
		for ch := range out.Data {
			copy(out.Data[ch][base:], chunk.Data[ch])
		}
	}
	return out, s.Comments(), nil
}
