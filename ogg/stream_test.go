package ogg

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
)

// muxPackets writes packets as one logical stream and returns the encoded bytes.
func muxPackets(t *testing.T, serial uint32, packets [][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	m := NewMuxer(&buf, serial)
	for i, p := range packets {
		if err := m.WritePacket(p, int64(i+1)*1024); err != nil {
			t.Fatalf("WritePacket %d: %v", i, err)
		}
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.Bytes()
}

// demuxAll reads every packet until EOF.
func demuxAll(t *testing.T, data []byte) []Packet {
	t.Helper()
	d := NewDemuxer(bytes.NewReader(data))
	var out []Packet
	for {
		p, err := d.ReadPacket()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		out = append(out, p)
	}
}

// TestPacketRoundTrip covers the sizes that exercise every lacing edge: empty packets, the 255
// boundary, and packets larger than one page can hold.
func TestPacketRoundTrip(t *testing.T) {
	sizes := []int{0, 1, 254, 255, 256, 510, 4095, 4096, 4097, MaxPayload - 1, MaxPayload, MaxPayload + 1, 200000}

	for _, n := range sizes {
		want := make([]byte, n)
		for i := range want {
			want[i] = byte(i * 7)
		}
		got := demuxAll(t, muxPackets(t, 0x1234, [][]byte{want}))
		if len(got) != 1 {
			t.Errorf("n=%d: got %d packets, want 1", n, len(got))
			continue
		}
		if !bytes.Equal(got[0].Data, want) {
			t.Errorf("n=%d: payload differs (%d bytes back)", n, len(got[0].Data))
		}
	}
}

func TestManyPacketsRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	packets := make([][]byte, 500)
	for i := range packets {
		p := make([]byte, rng.IntN(3000))
		for j := range p {
			p[j] = byte(rng.UintN(256))
		}
		packets[i] = p
	}

	got := demuxAll(t, muxPackets(t, 7, packets))
	if len(got) != len(packets) {
		t.Fatalf("got %d packets, want %d", len(got), len(packets))
	}
	for i := range packets {
		if !bytes.Equal(got[i].Data, packets[i]) {
			t.Fatalf("packet %d differs", i)
		}
		if got[i].Serial != 7 {
			t.Fatalf("packet %d has serial %#x", i, got[i].Serial)
		}
	}
}

func TestBOSAndEOSFlags(t *testing.T) {
	packets := [][]byte{[]byte("first"), []byte("middle"), []byte("last")}
	got := demuxAll(t, muxPackets(t, 1, packets))
	if len(got) != 3 {
		t.Fatalf("got %d packets, want 3", len(got))
	}
	if !got[0].FirstPage {
		t.Error("first packet not marked as starting the stream")
	}
	if got[1].FirstPage || got[2].FirstPage {
		t.Error("a later packet is marked as starting the stream")
	}
	if !got[2].LastPage {
		t.Error("last packet not marked as ending the stream")
	}
	if got[0].LastPage || got[1].LastPage {
		t.Error("an earlier packet is marked as ending the stream")
	}
}

// TestCloseCarriesFinalGranule pins the reason WritePacket flushes the previous page rather than the
// current one: the last packet must still be buffered when Close sets the end-of-stream flag.
func TestCloseCarriesFinalGranule(t *testing.T) {
	// Enough packets that the threshold triggers at least one intermediate page.
	packets := make([][]byte, 20)
	for i := range packets {
		packets[i] = make([]byte, 1000)
	}
	data := muxPackets(t, 3, packets)

	got := demuxAll(t, data)
	last := got[len(got)-1]
	if !last.LastPage {
		t.Fatal("final packet lost its end-of-stream flag")
	}
	want := int64(len(packets)) * 1024
	if last.GranulePos != want {
		t.Errorf("final granule = %d, want %d", last.GranulePos, want)
	}
}

// TestSpanningPacketPagesHaveNoGranule covers the framing rule that a page on which no packet ends
// must report -1 rather than 0.
func TestSpanningPacketPagesHaveNoGranule(t *testing.T) {
	big := make([]byte, MaxPayload*2+50)
	data := muxPackets(t, 9, [][]byte{big})

	r := NewReader(bytes.NewReader(data))
	var pages []*Page
	for {
		p, err := r.ReadPage()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadPage: %v", err)
		}
		pages = append(pages, p.Clone())
	}
	if len(pages) < 3 {
		t.Fatalf("got %d pages, want at least 3", len(pages))
	}

	for i, p := range pages {
		if p.EndsPacket() {
			continue
		}
		if p.GranulePos != NoGranule {
			t.Errorf("page %d ends no packet but reports granule %d", i, p.GranulePos)
		}
		if i > 0 && !p.Continued() {
			t.Errorf("page %d carries a packet tail but lacks the continued flag", i)
		}
	}
}

func TestFlushForcesPageBoundary(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer(&buf, 42)
	if err := m.WritePacket([]byte("header"), NoGranule); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}
	if err := m.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := m.WritePacket([]byte("audio"), 100); err != nil {
		t.Fatalf("WritePacket: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r := NewReader(bytes.NewReader(buf.Bytes()))
	first, err := r.ReadPage()
	if err != nil {
		t.Fatalf("ReadPage: %v", err)
	}
	if !bytes.Equal(first.Payload, []byte("header")) {
		t.Errorf("first page holds %q, want the flushed packet alone", first.Payload)
	}
}

// TestEOSLandsOnTheFinalDataPage covers a footgun that costs a truncated file: whether or not the
// caller flushes after the last packet, the end-of-stream flag and the final granule position must
// end up on the page carrying that packet, never on a trailing empty one.
func TestEOSLandsOnTheFinalDataPage(t *testing.T) {
	for _, flushLast := range []bool{false, true} {
		name := "no flush"
		if flushLast {
			name = "flush before close"
		}
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			m := NewMuxer(&buf, 21)
			for i := range 8 {
				if err := m.WritePacket(bytes.Repeat([]byte("z"), 2000), int64(i+1)*512); err != nil {
					t.Fatalf("WritePacket: %v", err)
				}
			}
			if flushLast {
				if err := m.Flush(); err != nil {
					t.Fatalf("Flush: %v", err)
				}
			}
			if err := m.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			pages := splitPages(t, buf.Bytes())
			last, err := unmarshalPage(pages[len(pages)-1])
			if err != nil {
				t.Fatalf("unmarshalPage: %v", err)
			}
			if !last.EOS() {
				t.Fatal("final page lacks the end-of-stream flag")
			}
			if len(last.Payload) == 0 {
				t.Error("stream ends with an empty page, so the final granule position is lost")
			}
			if last.GranulePos != 8*512 {
				t.Errorf("final granule = %d, want %d", last.GranulePos, 8*512)
			}
		})
	}
}

func TestFlushWhenEmptyIsNoop(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer(&buf, 1)
	if err := m.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("Flush with nothing buffered wrote %d bytes", buf.Len())
	}
}

func TestWriteAfterCloseFails(t *testing.T) {
	var buf bytes.Buffer
	m := NewMuxer(&buf, 1)
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := m.WritePacket([]byte("x"), 0); err == nil {
		t.Error("WritePacket succeeded after Close")
	}
}

// TestMultiplexedStreams checks that packets from interleaved logical streams stay separated.
func TestMultiplexedStreams(t *testing.T) {
	a := muxPackets(t, 0xAAAA, [][]byte{[]byte("a0"), []byte("a1"), []byte("a2")})
	b := muxPackets(t, 0xBBBB, [][]byte{[]byte("b0"), []byte("b1")})

	// Interleave at page granularity, keeping each stream's pages in order.
	pagesA, pagesB := splitPages(t, a), splitPages(t, b)
	var mixed []byte
	for i := 0; i < len(pagesA) || i < len(pagesB); i++ {
		if i < len(pagesA) {
			mixed = append(mixed, pagesA[i]...)
		}
		if i < len(pagesB) {
			mixed = append(mixed, pagesB[i]...)
		}
	}

	var gotA, gotB []string
	for _, p := range demuxAll(t, mixed) {
		switch p.Serial {
		case 0xAAAA:
			gotA = append(gotA, string(p.Data))
		case 0xBBBB:
			gotB = append(gotB, string(p.Data))
		default:
			t.Fatalf("unexpected serial %#x", p.Serial)
		}
	}
	if len(gotA) != 3 || gotA[0] != "a0" || gotA[2] != "a2" {
		t.Errorf("stream A = %v", gotA)
	}
	if len(gotB) != 2 || gotB[0] != "b0" || gotB[1] != "b1" {
		t.Errorf("stream B = %v", gotB)
	}
}

// TestChainedStreams checks that a fresh beginning-of-stream page after an end-of-stream page starts
// a new logical stream rather than confusing the old one.
func TestChainedStreams(t *testing.T) {
	first := muxPackets(t, 5, [][]byte{[]byte("one"), []byte("two")})
	second := muxPackets(t, 5, [][]byte{[]byte("three")})

	got := demuxAll(t, append(append([]byte(nil), first...), second...))
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("got %d packets, want %d", len(got), len(want))
	}
	for i := range want {
		if string(got[i].Data) != want[i] {
			t.Errorf("packet %d = %q, want %q", i, got[i].Data, want[i])
		}
	}
	if !got[2].FirstPage {
		t.Error("the chained stream's first packet is not marked as starting a stream")
	}
}

func TestResyncSkipsGarbage(t *testing.T) {
	clean := muxPackets(t, 11, [][]byte{[]byte("alpha"), []byte("beta")})
	garbage := bytes.Repeat([]byte("junk data not a page "), 20)

	dirty := append(append([]byte(nil), garbage...), clean...)
	d := NewDemuxer(bytes.NewReader(dirty))

	var got []string
	for {
		p, err := d.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		got = append(got, string(p.Data))
	}
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Errorf("got %v, want [alpha beta]", got)
	}
	if d.Skipped() != int64(len(garbage)) {
		t.Errorf("skipped %d bytes, want %d", d.Skipped(), len(garbage))
	}
}

// TestResyncPastCapturePatternInPayload covers the case that makes naive resync wrong: "OggS" is
// ordinary data when it appears inside a payload.
func TestResyncPastCapturePatternInPayload(t *testing.T) {
	payload := append([]byte("lead-in"), []byte(magic)...)
	payload = append(payload, []byte("trailing bytes")...)
	data := muxPackets(t, 13, [][]byte{payload})

	got := demuxAll(t, data)
	if len(got) != 1 || !bytes.Equal(got[0].Data, payload) {
		t.Fatalf("payload containing %q did not survive the round trip", magic)
	}
}

func TestCorruptPageIsSkipped(t *testing.T) {
	// Each packet must exceed pageThreshold, or the muxer packs them onto a single page and there
	// is no second page to corrupt.
	first := bytes.Repeat([]byte("a"), pageThreshold+1)
	second := bytes.Repeat([]byte("b"), pageThreshold+1)
	data := muxPackets(t, 17, [][]byte{first, second})
	pages := splitPages(t, data)
	if len(pages) < 2 {
		t.Fatalf("got %d pages, want at least 2", len(pages))
	}
	// Corrupt the last page's payload so its checksum fails.
	last := pages[len(pages)-1]
	last[len(last)-1] ^= 0xff

	var rebuilt []byte
	for _, p := range pages {
		rebuilt = append(rebuilt, p...)
	}
	d := NewDemuxer(bytes.NewReader(rebuilt))
	for {
		_, err := d.ReadPacket()
		if err != nil {
			break
		}
	}
	if d.Skipped() == 0 {
		t.Error("a page with a broken checksum was accepted")
	}
}

func TestTruncatedStreamStopsCleanly(t *testing.T) {
	data := muxPackets(t, 19, [][]byte{make([]byte, 20000)})
	for _, cut := range []int{1, 26, 27, 100, len(data) / 2, len(data) - 1} {
		d := NewDemuxer(bytes.NewReader(data[:cut]))
		for {
			_, err := d.ReadPacket()
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
					t.Errorf("cut at %d: %v", cut, err)
				}
				break
			}
		}
	}
}

// splitPages breaks an encoded stream into page images.
func splitPages(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for off := 0; off < len(data); {
		if string(data[off:off+4]) != magic {
			t.Fatalf("no page at offset %d", off)
		}
		nsegs := int(data[off+offSegments])
		total := headerFixed + nsegs
		for _, s := range data[off+headerFixed : off+headerFixed+nsegs] {
			total += int(s)
		}
		out = append(out, data[off:off+total])
		off += total
	}
	return out
}

func BenchmarkMuxDemux(b *testing.B) {
	packet := make([]byte, 800)
	b.SetBytes(int64(len(packet)))
	for b.Loop() {
		var buf bytes.Buffer
		m := NewMuxer(&buf, 1)
		for i := range 64 {
			_ = m.WritePacket(packet, int64(i))
		}
		_ = m.Close()

		d := NewDemuxer(bytes.NewReader(buf.Bytes()))
		for {
			if _, err := d.ReadPacket(); err != nil {
				break
			}
		}
	}
}
