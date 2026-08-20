package ogg

import (
	"bytes"
	"testing"
)

// TestPageRoundTripIsByteExact is why Page keeps its segment table: a payload can be laced in more
// than one valid way, so only preserving the table reproduces the original bytes.
func TestPageRoundTripIsByteExact(t *testing.T) {
	orig := decodePage(t, firstPage)
	image := append([]byte(nil), orig...)

	page, err := unmarshalPage(image)
	if err != nil {
		t.Fatalf("unmarshalPage: %v", err)
	}
	got, err := page.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !bytes.Equal(got, orig) {
		t.Errorf("round trip differs\n got %x\nwant %x", got, orig)
	}
}

func TestPageHeaderFields(t *testing.T) {
	page, err := unmarshalPage(decodePage(t, firstPage))
	if err != nil {
		t.Fatalf("unmarshalPage: %v", err)
	}
	if !page.BOS() {
		t.Error("BOS flag not set on the first page")
	}
	if page.EOS() || page.Continued() {
		t.Error("EOS or Continued set on the first page")
	}
	if page.Serial != 0x4b20f105 {
		t.Errorf("Serial = %#08x, want 0x4b20f105", page.Serial)
	}
	if page.Sequence != 0 {
		t.Errorf("Sequence = %d, want 0", page.Sequence)
	}
	if page.GranulePos != 0 {
		t.Errorf("GranulePos = %d, want 0", page.GranulePos)
	}
	if len(page.Payload) != 30 {
		t.Errorf("payload is %d bytes, want 30", len(page.Payload))
	}
	if !page.EndsPacket() {
		t.Error("EndsPacket false, but the identification header ends here")
	}
}

// TestLacing covers the rule that costs a day if missed: a packet whose length is a multiple of 255
// needs a trailing zero, or the reader treats it as continuing onto the next page.
func TestLacing(t *testing.T) {
	cases := []struct {
		n    int
		want []uint8
	}{
		{0, []uint8{0}},
		{1, []uint8{1}},
		{254, []uint8{254}},
		{255, []uint8{255, 0}},
		{256, []uint8{255, 1}},
		{509, []uint8{255, 254}},
		{510, []uint8{255, 255, 0}},
		{511, []uint8{255, 255, 1}},
	}
	for _, c := range cases {
		got := appendLacing(nil, c.n)
		if len(got) != lacingLen(c.n) {
			t.Errorf("n=%d: appendLacing gave %d entries, lacingLen said %d", c.n, len(got), lacingLen(c.n))
		}
		if len(got) != len(c.want) {
			t.Errorf("n=%d: got %v, want %v", c.n, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("n=%d: got %v, want %v", c.n, got, c.want)
				break
			}
		}
	}
}

func TestLacingSumsToPacketLength(t *testing.T) {
	for n := range 2000 {
		total := 0
		for _, s := range appendLacing(nil, n) {
			total += int(s)
		}
		if total != n {
			t.Fatalf("n=%d: lacing sums to %d", n, total)
		}
	}
}

func TestEndsPacket(t *testing.T) {
	cases := []struct {
		segs []uint8
		want bool
	}{
		{nil, false},
		{[]uint8{255}, false},
		{[]uint8{255, 255}, false},
		{[]uint8{0}, true},
		{[]uint8{255, 0}, true},
		{[]uint8{10, 255}, true},
	}
	for _, c := range cases {
		p := &Page{Segments: c.segs}
		if got := p.EndsPacket(); got != c.want {
			t.Errorf("segs=%v: EndsPacket = %v, want %v", c.segs, got, c.want)
		}
	}
}

func TestUnmarshalRejectsMalformed(t *testing.T) {
	good := decodePage(t, firstPage)

	t.Run("truncated", func(t *testing.T) {
		if _, err := unmarshalPage(good[:20]); err == nil {
			t.Error("accepted a header shorter than 27 bytes")
		}
	})
	t.Run("bad magic", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[0] = 'X'
		if _, err := unmarshalPage(bad); err == nil {
			t.Error("accepted a bad capture pattern")
		}
	})
	t.Run("bad version", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[offVersion] = 1
		if _, err := unmarshalPage(bad); err == nil {
			t.Error("accepted version 1")
		}
	})
	t.Run("corrupt payload", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[len(bad)-1] ^= 0xff
		if _, err := unmarshalPage(bad); err == nil {
			t.Error("accepted a page whose checksum no longer matches")
		}
	})
	t.Run("length mismatch", func(t *testing.T) {
		if _, err := unmarshalPage(good[:len(good)-1]); err == nil {
			t.Error("accepted an image shorter than the segment table describes")
		}
	})
}

func TestMarshalRejectsInconsistentPage(t *testing.T) {
	p := &Page{Segments: []uint8{10}, Payload: make([]byte, 5)}
	if _, err := p.Marshal(); err == nil {
		t.Error("accepted a segment table that does not sum to the payload length")
	}

	p = &Page{Segments: make([]uint8, MaxSegments+1)}
	if _, err := p.Marshal(); err == nil {
		t.Error("accepted more than 255 segments")
	}
}

func TestCloneSharesNoStorage(t *testing.T) {
	page, err := unmarshalPage(decodePage(t, firstPage))
	if err != nil {
		t.Fatalf("unmarshalPage: %v", err)
	}
	c := page.Clone()
	c.Payload[0] ^= 0xff
	c.Segments[0] ^= 0xff
	if page.Payload[0] == c.Payload[0] || page.Segments[0] == c.Segments[0] {
		t.Error("Clone shares storage with the original")
	}
}
