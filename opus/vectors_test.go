//go:build conformance

package opus

import (
	"encoding/binary"
	"math"
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

	// Every packet of the suite agrees. There is no partial credit here: a decoder that reads one
	// symbol differently reports a different state, so anything short of all of them is a fault.
	if matched != total {
		t.Errorf("%d of %d packets match; the suite is passed only in full", matched, total)
	}
}

// readDecoded reads a reference output: interleaved 16-bit samples at 48 kHz, two channels.
func readDecoded(t *testing.T, path string) [][]float32 {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	frames := len(raw) / 4
	out := [][]float32{make([]float32, frames), make([]float32, frames)}
	for i := range frames {
		out[0][i] = float32(int16(binary.LittleEndian.Uint16(raw[4*i:]))) / 32768
		out[1][i] = float32(int16(binary.LittleEndian.Uint16(raw[4*i+2:]))) / 32768
	}
	return out
}

// TestConformanceOfficialVectorAudio compares the decoded samples with the reference output.
//
// Two outputs are published for each bitstream. They differ only in whether the transform codec's
// optional 180-degree phase shift is applied to intensity-coded bands, which RFC 8251 section 10
// leaves to the decoder; passing either set is compliance. This reports against both and holds the
// closer one.
//
// The measure is a signal-to-noise ratio rather than a sample-for-sample match. The specification
// does not require bit-exactness of a floating-point decoder, and the reference output is itself
// quantised to sixteen bits, so an exact comparison would be measuring the quantiser.
func TestConformanceOfficialVectorAudio(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(vectorDir, "testvector*.bit"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skipf("no test vectors in %s; run `mise run fixtures:vectors`", vectorDir)
	}
	sort.Strings(files)

	worstSNR := math.Inf(1)
	checked := 0

	for _, path := range files {
		name := filepath.Base(path)
		base := name[:len(name)-len(".bit")]

		variants := map[string][][]float32{
			"":  readDecoded(t, filepath.Join(vectorDir, base+".dec")),
			"m": readDecoded(t, filepath.Join(vectorDir, base+"m.dec")),
		}
		if variants[""] == nil && variants["m"] == nil {
			t.Skipf("no reference output beside %s", name)
		}

		dec, err := NewDecoder(2)
		if err != nil {
			t.Fatal(err)
		}
		var ours [][]float32
		for _, p := range readVectorFile(t, path) {
			got, err := dec.Decode(p.data)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if ours == nil {
				ours = make([][]float32, len(got))
			}
			for c := range got {
				ours[c] = append(ours[c], got[c]...)
			}
		}

		best, bestName := math.Inf(-1), ""
		for suffix, ref := range variants {
			if ref == nil {
				continue
			}
			if snr := signalToNoise(ours, ref); snr > best {
				best, bestName = snr, suffix
			}
		}
		checked++
		worstSNR = math.Min(worstSNR, best)
		t.Logf("%-18s %6.1f dB against the %q output", name, best, bestName)
	}

	// A ratchet, not a target. Three of the twelve match the reference sample for sample and the
	// rest sit between 65 and 83 dB, which is single precision against double and nothing else. The
	// bar records where the decoder stands and must never fall.
	const floor = 60.0
	if worstSNR < floor {
		t.Errorf("worst vector reached %.1f dB, below the %.0f dB already held", worstSNR, floor)
	}
	t.Logf("%d vectors compared, worst %.1f dB", checked, worstSNR)
}

// signalToNoise returns the ratio of the reference's energy to the difference's, in decibels.
func signalToNoise(ours, ref [][]float32) float64 {
	var signal, noise float64
	n := min(len(ref[0]), len(ours[0]))
	if n == 0 {
		return math.Inf(-1)
	}
	for c := range ref {
		if c >= len(ours) {
			break
		}
		for i := range n {
			r := float64(ref[c][i])
			d := r - float64(ours[c][i])
			signal += r * r
			noise += d * d
		}
	}
	if noise == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(signal/noise)
}
