package ogg

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// FuzzDemuxer drives the parser with arbitrary bytes. The contract is that no input crashes, hangs
// or allocates without bound; malformed data must come back as an error.
func FuzzDemuxer(f *testing.F) {
	f.Add(mustHex(firstPage))
	f.Add([]byte(magic))
	f.Add([]byte("OggS\x00\x02"))
	f.Add([]byte{})
	f.Add(bytes.Repeat([]byte("OggS"), 64))

	var buf bytes.Buffer
	m := NewMuxer(&buf, 1)
	_ = m.WritePacket(bytes.Repeat([]byte("x"), 300), 48000)
	_ = m.WritePacket(nil, 96000)
	_ = m.Close()
	f.Add(buf.Bytes())

	f.Fuzz(func(t *testing.T, data []byte) {
		d := NewDemuxer(bytes.NewReader(data))
		for n := 0; ; n++ {
			if n > len(data)+16 {
				t.Fatalf("demuxer produced %d packets from %d bytes", n, len(data))
			}
			p, err := d.ReadPacket()
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
					errors.Is(err, ErrHole) || errors.Is(err, ErrBadPage) {
					if errors.Is(err, ErrHole) {
						continue
					}
					return
				}
				t.Fatalf("unexpected error kind: %v", err)
			}
			if len(p.Data) > len(data) {
				t.Fatalf("packet of %d bytes from %d bytes of input", len(p.Data), len(data))
			}
		}
	})
}

// FuzzPage exercises the page decoder directly, without the reader's resynchronisation in front of
// it, so malformed headers are reached rather than skipped.
func FuzzPage(f *testing.F) {
	f.Add(mustHex(firstPage))
	f.Add(make([]byte, headerFixed))

	f.Fuzz(func(t *testing.T, data []byte) {
		page, err := unmarshalPage(data)
		if err != nil {
			return
		}
		// A page that parsed must re-serialize to the bytes it came from.
		got, err := page.Marshal()
		if err != nil {
			t.Fatalf("page parsed but would not marshal: %v", err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("round trip differs\n got %x\nwant %x", got, data)
		}
	})
}

func BenchmarkReadPage(b *testing.B) {
	var buf bytes.Buffer
	m := NewMuxer(&buf, 1)
	for i := range 128 {
		_ = m.WritePacket(make([]byte, 700), int64(i))
	}
	_ = m.Close()
	data := buf.Bytes()

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		r := NewReader(bytes.NewReader(data))
		for {
			if _, err := r.ReadPage(); err != nil {
				break
			}
		}
	}
}
