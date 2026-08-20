package vorbis

import (
	"encoding/hex"
	"errors"
	"testing"

	"github.com/faide/goxiph/vorbiscomment"
)

// identHeader is the identification header packet from a 44.1 kHz stereo file produced by
// oggenc 1.4 (libVorbis), taken from the payload of the file's first page.
const identHeader = "01766f72626973000000000244ac00000000000080b5010000000000b801"

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return b
}

// TestParseInfoAgainstReferenceHeader checks the reader against bytes libVorbis produced, which is
// the only way to know the bit order is right.
func TestParseInfoAgainstReferenceHeader(t *testing.T) {
	info, err := ParseInfo(mustHex(t, identHeader))
	if err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}

	if info.Version != 0 {
		t.Errorf("Version = %d, want 0", info.Version)
	}
	if info.Channels != 2 {
		t.Errorf("Channels = %d, want 2", info.Channels)
	}
	if info.SampleRate != 44100 {
		t.Errorf("SampleRate = %d, want 44100", info.SampleRate)
	}
	// A misread of the block-size nibbles is the classic bit-order symptom: the values come back
	// swapped or as implausible powers of two.
	if info.BlockSize0 != 256 {
		t.Errorf("BlockSize0 = %d, want 256", info.BlockSize0)
	}
	if info.BlockSize1 != 2048 {
		t.Errorf("BlockSize1 = %d, want 2048", info.BlockSize1)
	}
	if info.BitrateNominal != 112000 {
		t.Errorf("BitrateNominal = %d, want 112000", info.BitrateNominal)
	}
	if info.BitrateMaximum != 0 || info.BitrateMinimum != 0 {
		t.Errorf("bitrate bounds = %d/%d, want 0/0", info.BitrateMaximum, info.BitrateMinimum)
	}
	if info.Format().SampleRate != 44100 || info.Format().Channels != 2 {
		t.Errorf("Format = %v", info.Format())
	}
}

func TestInfoRoundTrip(t *testing.T) {
	orig := mustHex(t, identHeader)
	info, err := ParseInfo(orig)
	if err != nil {
		t.Fatalf("ParseInfo: %v", err)
	}
	got, err := info.AppendTo(nil)
	if err != nil {
		t.Fatalf("AppendTo: %v", err)
	}
	if len(got) != len(orig) {
		t.Fatalf("re-encoded header is %d bytes, want %d", len(got), len(orig))
	}
	for i := range got {
		if got[i] != orig[i] {
			t.Fatalf("byte %d differs: got %#02x, want %#02x\n got %x\nwant %x", i, got[i], orig[i], got, orig)
		}
	}
}

func TestParseInfoRejects(t *testing.T) {
	good := mustHex(t, identHeader)

	cases := []struct {
		name string
		mut  func([]byte)
	}{
		{"bad signature", func(b []byte) { b[1] = 'X' }},
		{"wrong packet type", func(b []byte) { b[0] = packetComment }},
		{"nonzero version", func(b []byte) { b[7] = 1 }},
		{"zero channels", func(b []byte) { b[11] = 0 }},
		{"zero sample rate", func(b []byte) { b[12], b[13], b[14], b[15] = 0, 0, 0, 0 }},
		{"framing bit clear", func(b []byte) { b[len(b)-1] &^= 1 }},
		{"block sizes swapped", func(b []byte) { b[len(b)-2] = 0x27 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bad := append([]byte(nil), good...)
			c.mut(bad)
			if _, err := ParseInfo(bad); err == nil {
				t.Error("accepted an invalid header")
			}
		})
	}
}

func TestParseInfoRejectsTruncation(t *testing.T) {
	good := mustHex(t, identHeader)
	for n := range len(good) {
		if _, err := ParseInfo(good[:n]); err == nil {
			t.Errorf("accepted a header truncated to %d bytes", n)
		}
	}
}

func TestBlockExp(t *testing.T) {
	valid := []int{64, 128, 256, 512, 1024, 2048, 4096, 8192}
	for _, size := range valid {
		if _, err := blockExp(size); err != nil {
			t.Errorf("blockExp(%d): %v", size, err)
		}
	}
	for _, size := range []int{0, -256, 1, 32, 100, 3000, 16384} {
		if _, err := blockExp(size); err == nil {
			t.Errorf("blockExp(%d) accepted an invalid size", size)
		}
	}
}

func TestAppendToRejectsBadInfo(t *testing.T) {
	base := Info{Channels: 2, SampleRate: 44100, BlockSize0: 256, BlockSize1: 2048}

	bad := []struct {
		name string
		info Info
	}{
		{"zero channels", Info{Channels: 0, SampleRate: 44100, BlockSize0: 256, BlockSize1: 2048}},
		{"zero rate", Info{Channels: 2, SampleRate: 0, BlockSize0: 256, BlockSize1: 2048}},
		{"short exceeds long", Info{Channels: 2, SampleRate: 44100, BlockSize0: 2048, BlockSize1: 256}},
		{"non power of two", Info{Channels: 2, SampleRate: 44100, BlockSize0: 300, BlockSize1: 2048}},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			if _, err := c.info.AppendTo(nil); err == nil {
				t.Error("encoded an invalid header")
			}
		})
	}
	if _, err := base.AppendTo(nil); err != nil {
		t.Errorf("rejected a valid header: %v", err)
	}
}

func TestCommentRoundTrip(t *testing.T) {
	want := vorbiscomment.Tags{Vendor: "Xiph.Org libVorbis I 20200704"}
	want.Add("TITLE", "A Song")
	want.Add("ARTIST", "Someone")
	want.Add("ARTIST", "Someone Else")
	want.Add("DESCRIPTION", "unicode: héllo wörld ✓")

	packet := AppendComments(nil, want)
	got, err := ParseComments(packet)
	if err != nil {
		t.Fatalf("ParseComments: %v", err)
	}
	if got.Vendor != want.Vendor {
		t.Errorf("Vendor = %q, want %q", got.Vendor, want.Vendor)
	}
	if len(got.Comments) != len(want.Comments) {
		t.Fatalf("got %d comments, want %d", len(got.Comments), len(want.Comments))
	}
	for i := range want.Comments {
		if got.Comments[i] != want.Comments[i] {
			t.Errorf("comment %d = %q, want %q", i, got.Comments[i], want.Comments[i])
		}
	}

	// Repeated field names must survive in order, which a map-backed design would lose.
	artists := got.Get("artist")
	if len(artists) != 2 || artists[0] != "Someone" || artists[1] != "Someone Else" {
		t.Errorf("Get(artist) = %v", artists)
	}
	if got.First("TITLE") != "A Song" {
		t.Errorf("First(TITLE) = %q", got.First("TITLE"))
	}
	if got.First("absent") != "" {
		t.Error("First returned a value for a missing field")
	}
}

func TestParseCommentsRejects(t *testing.T) {
	var tags vorbiscomment.Tags
	tags.Add("TITLE", "x")
	good := AppendComments(nil, tags)

	t.Run("framing bit clear", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[len(bad)-1] = 0
		if _, err := ParseComments(bad); err == nil {
			t.Error("accepted a comment header with the framing bit clear")
		}
	})
	t.Run("wrong packet type", func(t *testing.T) {
		bad := append([]byte(nil), good...)
		bad[0] = packetIdent
		if _, err := ParseComments(bad); !errors.Is(err, ErrNotVorbis) {
			t.Errorf("got %v, want ErrNotVorbis", err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		for n := range len(good) - 1 {
			if _, err := ParseComments(good[:n]); err == nil {
				t.Errorf("accepted a header truncated to %d bytes", n)
			}
		}
	})
}

// TestCommentLengthIsNotTrusted covers the allocation guard: a 32-bit length field in an untrusted
// stream must not drive an allocation before the bytes are known to exist.
func TestCommentLengthIsNotTrusted(t *testing.T) {
	// vendor length claims 4 GiB with no data behind it
	packet := []byte{packetComment}
	packet = append(packet, headerSignature...)
	packet = append(packet, 0xFF, 0xFF, 0xFF, 0xFF)
	if _, err := ParseComments(packet); err == nil {
		t.Error("accepted a vendor length of 0xffffffff")
	}

	// comment count claims 100 million entries in a handful of bytes
	packet = []byte{packetComment}
	packet = append(packet, headerSignature...)
	packet = append(packet, 0, 0, 0, 0)             // empty vendor
	packet = append(packet, 0x00, 0xE1, 0xF5, 0x05) // 100,000,000 comments
	if _, err := ParseComments(packet); err == nil {
		t.Error("accepted a comment count of 100 million")
	}
}

func TestIsHeader(t *testing.T) {
	cases := []struct {
		data []byte
		want bool
	}{
		{[]byte{packetIdent}, true},
		{[]byte{packetComment}, true},
		{[]byte{packetSetup}, true},
		{[]byte{packetAudio}, false},
		{[]byte{0x02}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := IsHeader(c.data); got != c.want {
			t.Errorf("IsHeader(%v) = %v, want %v", c.data, got, c.want)
		}
	}
}

func FuzzParseInfo(f *testing.F) {
	b, _ := hex.DecodeString(identHeader)
	f.Add(b)
	f.Add([]byte{packetIdent})
	f.Fuzz(func(t *testing.T, data []byte) {
		info, err := ParseInfo(data)
		if err != nil {
			return
		}
		// Anything that parses must be re-encodable and must round-trip.
		out, err := info.AppendTo(nil)
		if err != nil {
			t.Fatalf("parsed header would not re-encode: %v", err)
		}
		again, err := ParseInfo(out)
		if err != nil {
			t.Fatalf("re-encoded header would not parse: %v", err)
		}
		if again != info {
			t.Fatalf("round trip changed the header: %+v vs %+v", again, info)
		}
	})
}

func FuzzParseComments(f *testing.F) {
	var tags vorbiscomment.Tags
	tags.Add("TITLE", "x")
	f.Add(AppendComments(nil, tags))
	f.Fuzz(func(t *testing.T, data []byte) {
		if _, err := ParseComments(data); err != nil {
			return
		}
	})
}
