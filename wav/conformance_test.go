//go:build conformance

package wav

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func needTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not installed", name)
	}
}

// TestConformanceReadsSoxOutput checks the reader against files a third party wrote, in every
// sample format the package claims to support.
func TestConformanceReadsSoxOutput(t *testing.T) {
	needTool(t, "sox")
	dir := t.TempDir()

	cases := []struct {
		name     string
		soxArgs  []string
		channels int
		depth    int
		isFloat  bool
	}{
		{"8-bit mono", []string{"-b", "8", "-e", "unsigned-integer", "-c", "1"}, 1, 8, false},
		{"16-bit stereo", []string{"-b", "16", "-e", "signed-integer", "-c", "2"}, 2, 16, false},
		{"24-bit stereo", []string{"-b", "24", "-e", "signed-integer", "-c", "2"}, 2, 24, false},
		{"32-bit stereo", []string{"-b", "32", "-e", "signed-integer", "-c", "2"}, 2, 32, false},
		{"32-bit float", []string{"-b", "32", "-e", "floating-point", "-c", "2"}, 2, 32, true},
		{"64-bit float", []string{"-b", "64", "-e", "floating-point", "-c", "2"}, 2, 64, true},
		{"six channels", []string{"-b", "16", "-e", "signed-integer", "-c", "6"}, 6, 16, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(c.name, " ", "_")+".wav")
			args := append([]string{"-n", "-r", "44100"}, c.soxArgs...)
			args = append(args, path, "synth", "0.05", "sine", "440")
			if out, err := exec.Command("sox", args...).CombinedOutput(); err != nil {
				t.Fatalf("sox: %v\n%s", err, out)
			}

			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()

			d, err := NewDecoder(f)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			got := d.Format()
			if got.Channels != c.channels {
				t.Errorf("Channels = %d, want %d", got.Channels, c.channels)
			}
			if got.BitsPerSample != c.depth {
				t.Errorf("BitsPerSample = %d, want %d", got.BitsPerSample, c.depth)
			}
			if got.Float != c.isFloat {
				t.Errorf("Float = %v, want %v", got.Float, c.isFloat)
			}
			if got.SampleRate != 44100 {
				t.Errorf("SampleRate = %d, want 44100", got.SampleRate)
			}

			// The samples must decode, and a 440 Hz tone is not silence.
			dst := make([][]float32, got.Channels)
			for i := range dst {
				dst[i] = make([]float32, 2205)
			}
			n, err := d.ReadFloat32(dst)
			if err != nil {
				t.Fatalf("ReadFloat32: %v", err)
			}
			if n == 0 {
				t.Fatal("read no frames")
			}
			var peak float32
			for _, v := range dst[0][:n] {
				if v > peak {
					peak = v
				}
			}
			if peak < 0.1 {
				t.Errorf("peak amplitude %v, want a recognisable tone", peak)
			}
		})
	}
}

// TestConformanceSamplesMatchSox decodes the same file through sox and through this package and
// requires identical integers.
func TestConformanceSamplesMatchSox(t *testing.T) {
	needTool(t, "sox")
	dir := t.TempDir()

	for _, depth := range []int{8, 16, 24, 32} {
		t.Run(fmt.Sprintf("%dbit", depth), func(t *testing.T) {
			path := filepath.Join(dir, fmt.Sprintf("t%d.wav", depth))
			enc := "signed-integer"
			if depth == 8 {
				enc = "unsigned-integer"
			}
			if out, err := exec.Command("sox", "-n", "-r", "44100", "-b", fmt.Sprint(depth),
				"-e", enc, "-c", "2", path, "synth", "0.05", "sine", "1000").CombinedOutput(); err != nil {
				t.Fatalf("sox: %v\n%s", err, out)
			}

			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			ours, format, err := DecodeAllInt32(f)
			f.Close()
			if err != nil {
				t.Fatalf("DecodeAllInt32: %v", err)
			}

			// sox writes the same samples as raw little-endian integers for comparison.
			rawPath := filepath.Join(dir, fmt.Sprintf("t%d.raw", depth))
			if out, err := exec.Command("sox", path, "-t", "raw", "-b", fmt.Sprint(depth),
				"-e", enc, rawPath).CombinedOutput(); err != nil {
				t.Fatalf("sox raw: %v\n%s", err, out)
			}
			raw, err := os.ReadFile(rawPath)
			if err != nil {
				t.Fatalf("read raw: %v", err)
			}

			width := depth / 8
			frames := len(raw) / width / format.Channels
			if frames != len(ours[0]) {
				t.Fatalf("decoded %d frames, sox wrote %d", len(ours[0]), frames)
			}
			for i := range frames {
				for ch := range format.Channels {
					want := decodeInt(raw[(i*format.Channels+ch)*width:], width)
					if ours[ch][i] != want {
						t.Fatalf("channel %d frame %d: got %d, want %d", ch, i, ours[ch][i], want)
					}
				}
			}
		})
	}
}

// TestConformanceOurOutputIsReadable checks that files this package writes are read correctly by
// ffprobe and by sox, which is the other half of interoperating.
func TestConformanceOurOutputIsReadable(t *testing.T) {
	needTool(t, "ffprobe")
	needTool(t, "sox")
	dir := t.TempDir()

	cases := []Format{
		{SampleRate: 44100, Channels: 1, BitsPerSample: 8},
		{SampleRate: 44100, Channels: 2, BitsPerSample: 16},
		{SampleRate: 48000, Channels: 2, BitsPerSample: 24},
		{SampleRate: 96000, Channels: 2, BitsPerSample: 32},
		{SampleRate: 44100, Channels: 2, BitsPerSample: 32, Float: true},
		{SampleRate: 48000, Channels: 6, BitsPerSample: 16, ChannelMask: 0x3F},
	}

	for _, format := range cases {
		name := fmt.Sprintf("%dch_%dbit", format.Channels, format.BitsPerSample)
		if format.Float {
			name += "_float"
		}
		t.Run(name, func(t *testing.T) {
			const n = 1000
			path := filepath.Join(dir, name+".wav")
			f, err := os.Create(path)
			if err != nil {
				t.Fatalf("create: %v", err)
			}

			e, err := NewEncoder(f, format)
			if err != nil {
				t.Fatalf("NewEncoder: %v", err)
			}
			samples := make([][]float32, format.Channels)
			for ch := range samples {
				samples[ch] = make([]float32, n)
				for i := range n {
					samples[ch][i] = float32(i%200-100) / 200
				}
			}
			if err := e.WriteFloat32(samples); err != nil {
				t.Fatalf("WriteFloat32: %v", err)
			}
			if err := e.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			f.Close()

			out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
				"-show_entries", "stream=sample_rate,channels", "-of", "csv=p=0", path).Output()
			if err != nil {
				t.Fatalf("ffprobe rejected our file: %v", err)
			}
			want := fmt.Sprintf("%d,%d", format.SampleRate, format.Channels)
			if !strings.HasPrefix(strings.TrimSpace(string(out)), want) {
				t.Errorf("ffprobe reports %q, want %q", strings.TrimSpace(string(out)), want)
			}

			// sox reading it without complaint is a second, independent opinion.
			if out, err := exec.Command("sox", "--i", path).CombinedOutput(); err != nil {
				t.Errorf("sox rejected our file: %v\n%s", err, out)
			}
		})
	}
}

// TestConformanceRoundTripThroughSox pushes our output through sox and back, requiring the samples
// to survive unchanged.
func TestConformanceRoundTripThroughSox(t *testing.T) {
	needTool(t, "sox")
	dir := t.TempDir()

	const n = 5000
	samples := [][]int32{make([]int32, n), make([]int32, n)}
	for ch := range samples {
		for i := range n {
			samples[ch][i] = int32((i*37+ch*11)%60000 - 30000)
		}
	}

	ours := filepath.Join(dir, "ours.wav")
	f, err := os.Create(ours)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	e, err := NewEncoder(f, Format{SampleRate: 44100, Channels: 2, BitsPerSample: 16})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := e.WriteInt32(samples); err != nil {
		t.Fatalf("WriteInt32: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f.Close()

	theirs := filepath.Join(dir, "theirs.wav")
	if out, err := exec.Command("sox", ours, theirs).CombinedOutput(); err != nil {
		t.Fatalf("sox: %v\n%s", err, out)
	}

	rf, err := os.Open(theirs)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer rf.Close()
	got, _, err := DecodeAllInt32(rf)
	if err != nil {
		t.Fatalf("DecodeAllInt32: %v", err)
	}

	if len(got[0]) != n {
		t.Fatalf("got %d frames back, want %d", len(got[0]), n)
	}
	for ch := range samples {
		for i := range n {
			if got[ch][i] != samples[ch][i] {
				t.Fatalf("channel %d frame %d: got %d, want %d", ch, i, got[ch][i], samples[ch][i])
			}
		}
	}
}

// TestConformanceWavToFlacToWav wires the two together, which is what the library is for.
func TestConformanceWavToFlacToWav(t *testing.T) {
	needTool(t, "ffmpeg")
	dir := t.TempDir()

	const n = 8000
	samples := [][]int32{make([]int32, n), make([]int32, n)}
	for ch := range samples {
		for i := range n {
			samples[ch][i] = int32((i*(ch+3))%40000 - 20000)
		}
	}

	wavPath := filepath.Join(dir, "in.wav")
	f, err := os.Create(wavPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	e, err := NewEncoder(f, Format{SampleRate: 44100, Channels: 2, BitsPerSample: 16})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := e.WriteInt32(samples); err != nil {
		t.Fatalf("WriteInt32: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	f.Close()

	// ffmpeg converts our WAVE to FLAC and back, and the samples must survive both hops.
	flacPath := filepath.Join(dir, "mid.flac")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-i", wavPath,
		"-c:a", "flac", flacPath).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg to flac: %v\n%s", err, out)
	}
	backPath := filepath.Join(dir, "back.wav")
	if out, err := exec.Command("ffmpeg", "-v", "error", "-y", "-i", flacPath,
		backPath).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg to wav: %v\n%s", err, out)
	}

	bf, err := os.Open(backPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer bf.Close()
	got, _, err := DecodeAllInt32(bf)
	if err != nil {
		t.Fatalf("DecodeAllInt32: %v", err)
	}
	if len(got[0]) != n {
		t.Fatalf("got %d frames, want %d", len(got[0]), n)
	}
	for ch := range samples {
		if !bytes.Equal(int32sToBytes(got[ch]), int32sToBytes(samples[ch])) {
			t.Fatalf("channel %d did not survive the round trip", ch)
		}
	}
}

func int32sToBytes(s []int32) []byte {
	b := make([]byte, len(s)*4)
	for i, v := range s {
		b[i*4] = byte(v)
		b[i*4+1] = byte(v >> 8)
		b[i*4+2] = byte(v >> 16)
		b[i*4+3] = byte(v >> 24)
	}
	return b
}
