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

// decodeFile runs a whole Ogg Vorbis file through the raw packet-level decoder, without the
// granule-position trimming the stream decoder applies.
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
// libvorbis produces, sample for sample and with the same length.
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
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			got, _, err := DecodeAll(f)
			f.Close()
			if err != nil {
				t.Fatalf("DecodeAll: %v", err)
			}

			want := referencePCM(t, path, got.Format.Channels)
			if len(want) == 0 || len(want[0]) == 0 {
				t.Skip("reference produced no samples")
			}

			// Granule-position trimming must land the length exactly, not approximately.
			if got.Frames() != len(want[0]) {
				t.Fatalf("decoded %d frames, reference has %d", got.Frames(), len(want[0]))
			}

			// One LSB of the 16-bit grid, which bounds the rounding disagreement between our
			// conversion and libvorbis's.
			const tolerance = 1.0 / 32768

			var worst float64
			worstAt, worstCh := 0, 0
			for c := range want {
				for i := range want[c] {
					d := math.Abs(quantize(got.Data[c][i]) - float64(want[c][i]))
					if d > worst {
						worst, worstAt, worstCh = d, i, c
					}
				}
			}
			if worst > tolerance {
				t.Errorf("max abs error %g at channel %d sample %d, want at most one LSB (%g)",
					worst, worstCh, worstAt, tolerance)
			} else {
				t.Logf("%d frames, max abs error %g", got.Frames(), worst)
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
