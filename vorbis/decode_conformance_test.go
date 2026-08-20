//go:build conformance

package vorbis

import (
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/faide/goxiph/audio"
	"github.com/faide/goxiph/ogg"
)

// decodeFile runs a whole Ogg Vorbis file through our decoder.
func decodeFile(t *testing.T, path string) (*audio.Buffer, Info) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	d := NewDecoder()
	dem := ogg.NewDemuxer(f)
	var out *audio.Buffer
	headers := 0
	var serial uint32

	for {
		p, err := dem.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		if headers == 0 {
			serial = p.Serial
		} else if p.Serial != serial {
			continue
		}

		switch headers {
		case 0:
			info, err := ParseInfo(p.Data)
			if err != nil {
				t.Fatalf("ParseInfo: %v", err)
			}
			d.SetInfo(info)
			out, err = audio.NewBuffer(info.Format(), 0)
			if err != nil {
				t.Fatalf("NewBuffer: %v", err)
			}
			headers++
		case 1:
			headers++ // comment header carries nothing the decoder needs
		case 2:
			s, err := ParseSetup(p.Data, d.Info())
			if err != nil {
				t.Fatalf("ParseSetup: %v", err)
			}
			if err := d.SetSetup(s); err != nil {
				t.Fatalf("SetSetup: %v", err)
			}
			headers++
		default:
			if err := d.DecodePacket(p.Data, out); err != nil {
				t.Fatalf("DecodePacket: %v", err)
			}
		}
	}
	return out, d.Info()
}

// referencePCM decodes a file with oggdec and returns interleaved samples per channel.
func referencePCM(t *testing.T, path string, channels int) [][]float32 {
	t.Helper()
	wav := filepath.Join(t.TempDir(), "ref.wav")
	if out, err := exec.Command("oggdec", "-Q", "-o", wav, path).CombinedOutput(); err != nil {
		t.Fatalf("oggdec: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(wav)
	if err != nil {
		t.Fatalf("read wav: %v", err)
	}

	// Locate the data chunk rather than assuming a fixed header size.
	off := 12
	for off+8 <= len(raw) {
		id := string(raw[off : off+4])
		size := int(binary.LittleEndian.Uint32(raw[off+4 : off+8]))
		if id == "data" {
			raw = raw[off+8 : min(off+8+size, len(raw))]
			break
		}
		off += 8 + size + size%2
	}

	frames := len(raw) / 2 / channels
	out := make([][]float32, channels)
	for c := range out {
		out[c] = make([]float32, frames)
	}
	for i := range frames {
		for c := range channels {
			v := int16(binary.LittleEndian.Uint16(raw[(i*channels+c)*2:]))
			out[c][i] = float32(v) / 32768
		}
	}
	return out
}

// TestConformanceDecodeMatchesReference is the gate for the whole decoder: our PCM must match what
// libvorbis produces.
//
// The comparison searches a small alignment window because our decoder emits every lapped sample
// while libvorbis trims the stream using the granule positions, which is a separate concern.
//
// Both sides are quantised to the 16-bit grid before comparing. Vorbis is a lossy transform codec
// and its output legitimately overshoots full scale, so oggdec's 16-bit output saturates where our
// float output does not. Comparing raw floats against saturated integers reports a decoder fault
// where there is none.
func TestConformanceDecodeMatchesReference(t *testing.T) {
	if _, err := exec.LookPath("oggdec"); err != nil {
		t.Skip("oggdec not installed")
	}

	for _, path := range corpus(t, "*hz_*ch.ogg") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			got, info := decodeFile(t, path)
			want := referencePCM(t, path, info.Channels)

			if got.Frames() == 0 {
				t.Fatal("decoder produced no samples")
			}
			if len(want) == 0 || len(want[0]) == 0 {
				t.Skip("reference produced no samples")
			}

			bestLag, bestErr := 0, math.Inf(1)
			for lag := -info.BlockSize1; lag <= info.BlockSize1; lag++ {
				if e := compareAt(got, want, lag); e < bestErr {
					bestErr, bestLag = e, lag
				}
			}

			// One LSB of the 16-bit grid, which bounds the rounding disagreement between our
			// conversion and libvorbis's.
			const tolerance = 1.0 / 32768
			if bestErr > tolerance {
				t.Errorf("best alignment lag %d gives max abs error %g, want at most one LSB (%g)\n"+
					"ours %d frames, reference %d frames",
					bestLag, bestErr, tolerance, got.Frames(), len(want[0]))
			} else {
				t.Logf("lag %d, max abs error %g over %d frames", bestLag, bestErr, len(want[0]))
			}
		})
	}
}

// quantize rounds a sample onto the 16-bit grid with saturation, matching what any 16-bit sink does
// to the decoder's float output.
func quantize(v float32) float64 {
	i := math.Round(float64(v) * 32768)
	return min(max(i, -32768), 32767) / 32768
}

// compareAt returns the maximum absolute difference with ours shifted by lag.
func compareAt(got *audio.Buffer, want [][]float32, lag int) float64 {
	n := len(want[0])
	// Skip the outer edges, where trimming differences dominate.
	start := max(0, -lag) + 64
	end := n - 64
	if end-start < 256 {
		return math.Inf(1)
	}

	var worst float64
	for c := range want {
		if c >= len(got.Data) {
			return math.Inf(1)
		}
		ours := got.Data[c]
		for i := start; i < end; i++ {
			j := i + lag
			if j < 0 || j >= len(ours) {
				return math.Inf(1)
			}
			d := math.Abs(quantize(ours[j]) - float64(want[c][i]))
			if d > worst {
				worst = d
			}
		}
	}
	return worst
}
