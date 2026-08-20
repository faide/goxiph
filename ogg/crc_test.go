package ogg

import (
	"encoding/binary"
	"encoding/hex"
	"hash/crc32"
	"testing"
)

// firstPage is the BOS page of a 44.1 kHz stereo file produced by oggenc 1.4 (libVorbis),
// carrying the Vorbis identification header. Stored CRC is at bytes 22:26.
const firstPage = "4f6767530002000000000000000005f1204b000000003e559b24011e" +
	"01766f72626973000000000244ac00000000000080b5010000000000b801"

// mustHex decodes a fixture. It panics rather than taking a *testing.T so that fuzz seed corpora,
// which are built outside a running test, can use it too.
func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic("bad fixture: " + err.Error())
	}
	return b
}

func decodePage(t *testing.T, s string) []byte {
	t.Helper()
	return mustHex(s)
}

// TestCRCAgainstReferencePage is the gate for the whole container: a wrong CRC invalidates every
// page we ever write, and the error is invisible until an external decoder rejects the file.
func TestCRCAgainstReferencePage(t *testing.T) {
	page := decodePage(t, firstPage)
	if len(page) != 58 {
		t.Fatalf("fixture is %d bytes, want 58", len(page))
	}

	want := binary.LittleEndian.Uint32(page[22:26])
	if want != 0x249b553e {
		t.Fatalf("fixture stored CRC = %#08x, want 0x249b553e", want)
	}

	binary.LittleEndian.PutUint32(page[22:26], 0)
	if got := crcOf(page); got != want {
		t.Errorf("crcOf = %#08x, want %#08x", got, want)
	}
}

// TestCRCIsNotReflected pins the property that makes hash/crc32 unusable here. If this ever starts
// passing, someone has swapped in a reflected implementation and every page will be wrong.
func TestCRCIsNotReflected(t *testing.T) {
	page := decodePage(t, firstPage)
	binary.LittleEndian.PutUint32(page[22:26], 0)

	reflected := crc32.Checksum(page, crc32.MakeTable(crc32.IEEE))
	if reflected == crcOf(page) {
		t.Fatal("Ogg CRC matched reflected CRC-32; the polynomial or reflection is wrong")
	}
}

func TestCRCUpdateIsIncremental(t *testing.T) {
	page := decodePage(t, firstPage)
	binary.LittleEndian.PutUint32(page[22:26], 0)

	whole := crcOf(page)
	for _, split := range []int{0, 1, 27, 40, len(page)} {
		piece := crcUpdate(0, page[:split])
		if got := crcUpdate(piece, page[split:]); got != whole {
			t.Errorf("split at %d: %#08x, want %#08x", split, got, whole)
		}
	}
}

func TestCRCOfEmpty(t *testing.T) {
	if got := crcOf(nil); got != 0 {
		t.Errorf("crcOf(nil) = %#08x, want 0", got)
	}
}

func BenchmarkCRC(b *testing.B) {
	buf := make([]byte, 65025) // maximum Ogg page payload
	b.SetBytes(int64(len(buf)))
	for b.Loop() {
		crcUpdate(0, buf)
	}
}
