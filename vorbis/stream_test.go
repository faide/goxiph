package vorbis

import (
	"testing"

	"github.com/faide/goxiph/ogg"
)

// TestApplyTrimLastPageWins is a regression test.
//
// A stream short enough to keep all its audio on one page reaches its final packet having seen no
// earlier granule position, so it is both the first page and the last. Taking the first-page branch
// there trims the surplus off the head: the sample count comes out right and every sample is wrong.
func TestApplyTrimLastPageWins(t *testing.T) {
	s := &StreamDecoder{decoded: 66240}
	p := ogg.Packet{GranulePos: 66150, LastPage: true}

	start, end := s.applyTrim(p, 0, 1024)
	if start != 0 {
		t.Errorf("start = %d, want 0; the surplus belongs at the tail", start)
	}
	if end != 1024-90 {
		t.Errorf("end = %d, want %d", end, 1024-90)
	}
	if s.decoded != 66150 {
		t.Errorf("decoded = %d, want 66150", s.decoded)
	}
}

func TestApplyTrimNoGranuleIsUntouched(t *testing.T) {
	s := &StreamDecoder{decoded: 5000}
	p := ogg.Packet{GranulePos: ogg.NoGranule}

	start, end := s.applyTrim(p, 0, 1024)
	if start != 0 || end != 1024 {
		t.Errorf("got [%d,%d), want [0,1024)", start, end)
	}
	if s.seenFirst {
		t.Error("a packet without a granule position marked the first page as seen")
	}
}

func TestApplyTrimFirstPageTrimsHead(t *testing.T) {
	// A first page that is not the last: the surplus is leading padding.
	s := &StreamDecoder{decoded: 1100}
	p := ogg.Packet{GranulePos: 1000, LastPage: false}

	start, end := s.applyTrim(p, 0, 1024)
	if start != 100 {
		t.Errorf("start = %d, want 100", start)
	}
	if end != 1024 {
		t.Errorf("end = %d, want 1024", end)
	}
	if s.decoded != 1000 {
		t.Errorf("decoded = %d, want 1000", s.decoded)
	}
	if s.frontTrim != 0 {
		t.Errorf("frontTrim = %d, want 0", s.frontTrim)
	}
}

// TestApplyTrimFrontSpillover covers a leading trim larger than the chunk it lands in, which has to
// carry over to the packets that follow.
func TestApplyTrimFrontSpillover(t *testing.T) {
	s := &StreamDecoder{decoded: 1500}
	p := ogg.Packet{GranulePos: 1000, LastPage: false}

	start, end := s.applyTrim(p, 0, 200)
	if start != 200 || end != 200 {
		t.Errorf("got [%d,%d), want an empty range", start, end)
	}
	if s.frontTrim != 300 {
		t.Errorf("frontTrim = %d, want 300 carried to the next packet", s.frontTrim)
	}

	// The next packet absorbs the rest.
	start, end = s.applyTrim(ogg.Packet{GranulePos: ogg.NoGranule}, 0, 1024)
	if start != 300 || end != 1024 {
		t.Errorf("got [%d,%d), want [300,1024)", start, end)
	}
	if s.frontTrim != 0 {
		t.Errorf("frontTrim = %d, want 0", s.frontTrim)
	}
}

func TestApplyTrimExactFitIsUntouched(t *testing.T) {
	s := &StreamDecoder{decoded: 1024}
	p := ogg.Packet{GranulePos: 1024, LastPage: true}

	start, end := s.applyTrim(p, 0, 1024)
	if start != 0 || end != 1024 {
		t.Errorf("got [%d,%d), want the whole chunk", start, end)
	}
}

// TestApplyTrimHugeTailTrimClampsToChunk guards against a granule position that would trim more than
// the final chunk holds.
func TestApplyTrimHugeTailTrimClampsToChunk(t *testing.T) {
	s := &StreamDecoder{decoded: 10000}
	p := ogg.Packet{GranulePos: 1, LastPage: true}

	start, end := s.applyTrim(p, 0, 512)
	if end < start {
		t.Errorf("got [%d,%d), an inverted range", start, end)
	}
	if end != 0 {
		t.Errorf("end = %d, want 0", end)
	}
}
