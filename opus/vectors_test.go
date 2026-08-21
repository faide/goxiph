//go:build conformance

package opus

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

const vectorDir = "../testdata/vectors"

// vectorPacket is one entry of a test vector file: a packet and the entropy coder state the
// reference decoder ends it with.
type vectorPacket struct {
	data       []byte
	finalRange uint32
}

// readVectorFile parses the format opus_demo writes: for each packet, its length and the decoder's
// final range, both big-endian, followed by the packet.
func readVectorFile(t *testing.T, path string) []vectorPacket {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	var out []vectorPacket
	for i := 0; i+8 <= len(raw); {
		length := int(binary.BigEndian.Uint32(raw[i:]))
		rng := binary.BigEndian.Uint32(raw[i+4:])
		i += 8
		if length <= 0 || i+length > len(raw) {
			t.Fatalf("%s: packet at %d claims %d bytes, %d remain", path, i, length, len(raw)-i)
		}
		out = append(out, vectorPacket{data: raw[i : i+length], finalRange: rng})
		i += length
	}
	return out
}

// TestConformanceOfficialVectorRange checks the decoder against the test vectors RFC 8251 names.
//
// These are the codec's own conformance suite, and they reach configurations no encoder driven by
// this project produces: medium-band SILK, every mode and bandwidth in both channel counts, mode
// switches within a stream, and the redundancy a lossy transport asks for. The corpus generated here
// covers eight of the eighteen mode and bandwidth combinations; these cover all of them.
//
// The check is on the entropy coder's final state, which each vector stores per packet. It is a
// running function of every symbol read, so it agrees only if the whole packet was read alike, and
// it names the packet where two decoders first part company.
func TestConformanceOfficialVectorRange(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(vectorDir, "testvector*.bit"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skipf("no test vectors in %s; run `mise run fixtures:vectors`", vectorDir)
	}
	sort.Strings(files)

	total, matched := 0, 0
	perFile := map[string][2]int{}

	for _, path := range files {
		name := filepath.Base(path)
		packets := readVectorFile(t, path)

		// The vectors are decoded at two channels, which is what their reference output holds.
		dec, err := NewDecoder(2)
		if err != nil {
			t.Fatal(err)
		}

		fileMatched := 0
		for _, p := range packets {
			total++
			if _, err := dec.Decode(p.data); err != nil {
				continue
			}
			if dec.FinalRange() == p.finalRange {
				fileMatched++
				matched++
			}
		}
		perFile[name] = [2]int{fileMatched, len(packets)}
	}

	for _, path := range files {
		name := filepath.Base(path)
		c := perFile[name]
		t.Logf("%-18s %5d/%5d", name, c[0], c[1])
	}
	t.Logf("%d of %d packets match the official final range (%.1f%%)",
		matched, total, 100*float64(matched)/float64(total))

	// The decoder does not pass the suite yet, and the bar records where it stands rather than
	// pretending otherwise. Raise it as the remaining divergences are closed; it must never fall.
	const passing = 18000
	if matched < passing {
		t.Errorf("%d packets match, down from the %d already reached", matched, passing)
	}
}
