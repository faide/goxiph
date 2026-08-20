//go:build conformance

package flac

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

const corpusDir = "../testdata/generated"

func corpus(t *testing.T, glob string) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(corpusDir, glob))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Skipf("no %s in %s; run `mise run fixtures`", glob, corpusDir)
	}
	return files
}

// referencePCM decodes a file with ffmpeg into planar int32 at the given depth.
//
// ffmpeg rather than the flac tool: libFLAC is BSD but the flac and metaflac binaries are GPLv2,
// and ffmpeg is the license-clean oracle that is present anyway.
func referencePCM(t *testing.T, path string, channels, depth int) [][]int32 {
	t.Helper()

	format, width := "s16le", 2
	if depth > 16 {
		format, width = "s32le", 4
	}
	out, err := exec.Command("ffmpeg", "-v", "error", "-i", path,
		"-f", format, "-c:a", "pcm_"+format, "-").Output()
	if err != nil {
		t.Fatalf("ffmpeg: %v", err)
	}

	frames := len(out) / width / channels
	res := make([][]int32, channels)
	for c := range res {
		res[c] = make([]int32, frames)
	}
	// ffmpeg widens samples to the container format, so a 24-bit stream arrives left-shifted into
	// 32 bits. Shifting back recovers the coded values.
	shift := 0
	if depth > 16 && depth < 32 {
		shift = 32 - depth
	}
	for i := range frames {
		for c := range channels {
			off := (i*channels + c) * width
			var v int32
			if width == 2 {
				v = int32(int16(binary.LittleEndian.Uint16(out[off:])))
			} else {
				v = int32(binary.LittleEndian.Uint32(out[off:]))
				v >>= shift
			}
			res[c][i] = v
		}
	}
	return res
}

// TestConformanceDecodeIsBitExact is the gate for the FLAC decoder.
//
// FLAC is lossless, so there is no tolerance to argue about: every sample matches or the decoder is
// wrong. That makes this a far stronger check than any lossy codec can have.
func TestConformanceDecodeIsBitExact(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	files := append(corpus(t, "*.flac"), corpus(t, "*.oga")...)
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer f.Close()

			got, info, err := DecodeAll(f)
			if err != nil {
				t.Fatalf("DecodeAll: %v", err)
			}
			want := referencePCM(t, path, info.Channels, info.BitsPerSample)

			if len(got) != len(want) {
				t.Fatalf("decoded %d channels, reference has %d", len(got), len(want))
			}
			if len(got) == 0 {
				t.Fatal("no channels")
			}
			if len(got[0]) != len(want[0]) {
				t.Fatalf("decoded %d frames, reference has %d", len(got[0]), len(want[0]))
			}
			// The stream info block declares the length; a decoder that agrees with the reference
			// but not with the header has still lost track of something.
			if info.TotalSamples != 0 && uint64(len(got[0])) != info.TotalSamples {
				t.Errorf("decoded %d frames, stream info declares %d", len(got[0]), info.TotalSamples)
			}

			for c := range got {
				for i := range got[c] {
					if got[c][i] != want[c][i] {
						t.Fatalf("channel %d sample %d: got %d, want %d (%d bit, %d Hz)",
							c, i, got[c][i], want[c][i], info.BitsPerSample, info.SampleRate)
					}
				}
			}
			t.Logf("%d frames, %d ch, %d bit, %d Hz, bit-exact",
				len(got[0]), info.Channels, info.BitsPerSample, info.SampleRate)
		})
	}
}

// TestConformanceStreamInfoAgainstFFprobe cross-checks the metadata against ffmpeg's own reader.
func TestConformanceStreamInfoAgainstFFprobe(t *testing.T) {
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe not installed")
	}

	for _, path := range corpus(t, "*hz_*ch.flac") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			out, err := exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
				"-show_entries", "stream=sample_rate,channels", "-of", "csv=p=0", path).Output()
			if err != nil {
				t.Fatalf("ffprobe: %v", err)
			}
			var rate, channels int
			if _, err := fmtSscan(string(out), &rate, &channels); err != nil {
				t.Skipf("could not parse ffprobe output %q", out)
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
			info := d.StreamInfo()
			if info.SampleRate != rate || info.Channels != channels {
				t.Errorf("we read %d Hz %d ch, ffprobe reports %d Hz %d ch",
					info.SampleRate, info.Channels, rate, channels)
			}
		})
	}
}

// fmtSscan parses "rate,channels" without pulling in a scanner that would trip on the trailing
// newline ffprobe emits.
func fmtSscan(s string, rate, channels *int) (int, error) {
	var cur int
	fields := 0
	dst := []*int{rate, channels}
	for _, ch := range s {
		switch {
		case ch >= '0' && ch <= '9':
			cur = cur*10 + int(ch-'0')
		case ch == ',' || ch == '\n':
			if fields < len(dst) {
				*dst[fields] = cur
			}
			fields++
			cur = 0
		}
	}
	if fields < 2 {
		return fields, strconvErr()
	}
	return fields, nil
}

func strconvErr() error { return strconv.ErrSyntax }

// TestConformanceOggAndNativeAgree decodes the same audio through both framings.
//
// Native FLAC finds frame boundaries by scanning for sync codes; Ogg delimits every packet. The two
// paths share everything after that point, so identical output confirms the framing rather than the
// codec.
func TestConformanceOggAndNativeAgree(t *testing.T) {
	for _, oggPath := range corpus(t, "*.oga") {
		base := filepath.Base(oggPath)
		nativePath := filepath.Join(corpusDir, base[:len(base)-len(".oga")]+".flac")
		if _, err := os.Stat(nativePath); err != nil {
			t.Skipf("no native counterpart for %s", base)
		}

		t.Run(base, func(t *testing.T) {
			of, err := os.Open(oggPath)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer of.Close()
			fromOgg, oggInfo, err := DecodeAll(of)
			if err != nil {
				t.Fatalf("decoding Ogg-FLAC: %v", err)
			}

			nf, err := os.Open(nativePath)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer nf.Close()
			fromNative, nativeInfo, err := DecodeAll(nf)
			if err != nil {
				t.Fatalf("decoding native FLAC: %v", err)
			}

			if oggInfo.Channels != nativeInfo.Channels ||
				oggInfo.SampleRate != nativeInfo.SampleRate ||
				oggInfo.BitsPerSample != nativeInfo.BitsPerSample {
				t.Fatalf("stream info differs: ogg %+v, native %+v", oggInfo, nativeInfo)
			}
			if len(fromOgg) != len(fromNative) || len(fromOgg[0]) != len(fromNative[0]) {
				t.Fatalf("ogg gave %d channels of %d frames, native gave %d of %d",
					len(fromOgg), len(fromOgg[0]), len(fromNative), len(fromNative[0]))
			}
			for c := range fromOgg {
				for i := range fromOgg[c] {
					if fromOgg[c][i] != fromNative[c][i] {
						t.Fatalf("channel %d sample %d: ogg %d, native %d",
							c, i, fromOgg[c][i], fromNative[c][i])
					}
				}
			}
		})
	}
}

// TestConformanceOggMappingHeader checks the first packet against the shape RFC 9639 section 10.1
// requires, including that it sits alone on a 79-byte page.
func TestConformanceOggMappingHeader(t *testing.T) {
	for _, path := range corpus(t, "*.oga") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(raw) < 79 {
				t.Fatalf("file is %d bytes", len(raw))
			}
			// The specification fixes the first page at 79 bytes because the mapping header must
			// not share a page.
			nsegs := int(raw[26])
			total := 27 + nsegs
			for _, s := range raw[27 : 27+nsegs] {
				total += int(s)
			}
			if total != 79 {
				t.Errorf("first page is %d bytes, want 79", total)
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
			if d.dem == nil {
				t.Error("an Ogg file was not recognised as Ogg-encapsulated")
			}
			if err := d.StreamInfo().Validate(); err != nil {
				t.Errorf("stream info from the mapping header is invalid: %v", err)
			}
		})
	}
}

// TestConformanceEncodeAcceptedByFFmpeg is the encoder's external gate.
//
// Our own decoder round-tripping proves the two agree; it cannot prove either matches the format.
// ffmpeg decoding our output to the exact input samples does.
func TestConformanceEncodeAcceptedByFFmpeg(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}

	cases := []struct {
		name     string
		channels int
		depth    int
		rate     int
		gen      func(i, ch int) int32
	}{
		{"silence", 1, 16, 44100, func(int, int) int32 { return 0 }},
		{"sine mono", 1, 16, 44100, func(i, _ int) int32 {
			return int32(20000 * math.Sin(float64(i)*0.02))
		}},
		{"sine stereo", 2, 16, 48000, func(i, ch int) int32 {
			return int32(18000 * math.Sin(float64(i)*0.01+float64(ch)*1.7))
		}},
		{"correlated stereo", 2, 16, 44100, func(i, ch int) int32 {
			v := int32(15000 * math.Sin(float64(i)*0.005))
			return v + int32(ch)
		}},
		{"deep", 2, 24, 96000, func(i, ch int) int32 {
			return int32(4000000 * math.Sin(float64(i)*0.003+float64(ch)))
		}},
		{"incompressible", 2, 16, 44100, func(i, ch int) int32 {
			x := uint32(i*2654435761 + ch*40503)
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			return int32(int16(x))
		}},
		{"wasted bits", 1, 16, 44100, func(i, _ int) int32 { return int32(i%512) << 5 }},
		{"five channels", 5, 16, 44100, func(i, ch int) int32 {
			return int32((i*(ch+3))%9000 - 4500)
		}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			const n = 30000
			samples := make([][]int32, c.channels)
			for ch := range samples {
				samples[ch] = make([]int32, n)
				for i := range n {
					samples[ch][i] = c.gen(i, ch)
				}
			}

			var buf seekableBuffer
			info := StreamInfo{SampleRate: c.rate, Channels: c.channels, BitsPerSample: c.depth}
			enc, err := NewEncoder(&buf, info, EncoderOptions{})
			if err != nil {
				t.Fatalf("NewEncoder: %v", err)
			}
			if err := enc.Write(samples); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if err := enc.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			path := filepath.Join(t.TempDir(), "ours.flac")
			if err := os.WriteFile(path, buf.data, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}

			// ffmpeg verifies the MD5 in the stream info block against the samples it decodes, so a
			// clean run also confirms the checksum we wrote.
			out, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-f", "null", "-").CombinedOutput()
			if err != nil {
				t.Fatalf("ffmpeg rejected our stream: %v\n%s", err, out)
			}
			if len(bytes.TrimSpace(out)) != 0 {
				t.Errorf("ffmpeg reported: %s", out)
			}

			want := referencePCM(t, path, c.channels, c.depth)
			if len(want) != c.channels || len(want[0]) != n {
				t.Fatalf("ffmpeg decoded %d channels of %d frames, want %d of %d",
					len(want), len(want[0]), c.channels, n)
			}
			for ch := range samples {
				for i := range n {
					if want[ch][i] != samples[ch][i] {
						t.Fatalf("channel %d sample %d: ffmpeg decoded %d, we encoded %d",
							ch, i, want[ch][i], samples[ch][i])
					}
				}
			}
			t.Logf("%d bytes for %d raw (%.1f%%)",
				len(buf.data), n*c.channels*(c.depth/8),
				100*float64(len(buf.data))/float64(n*c.channels*(c.depth/8)))
		})
	}
}

// TestConformanceCompressionRatio guards the encoder's efficiency against regression.
//
// A round trip proves correctness and says nothing about size: an encoder that emitted verbatim
// subframes throughout would pass every other test here. The comparison is against ffmpeg's own
// output for the same audio, aggregated so one awkward signal cannot swing the result.
func TestConformanceCompressionRatio(t *testing.T) {
	var ourTotal, theirTotal, rawTotal int64

	for _, path := range corpus(t, "*hz_*ch.flac") {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		samples, info, err := DecodeAll(f)
		f.Close()
		if err != nil {
			t.Fatalf("DecodeAll: %v", err)
		}

		var buf seekableBuffer
		enc, err := NewEncoder(&buf, StreamInfo{
			SampleRate:    info.SampleRate,
			Channels:      info.Channels,
			BitsPerSample: info.BitsPerSample,
		}, EncoderOptions{})
		if err != nil {
			t.Fatalf("NewEncoder: %v", err)
		}
		if err := enc.Write(samples); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if err := enc.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		ourTotal += int64(len(buf.data))
		theirTotal += st.Size()
		rawTotal += int64(len(samples[0]) * info.Channels * ((info.BitsPerSample + 7) / 8))
	}

	if rawTotal == 0 {
		t.Skip("no corpus")
	}
	vsRaw := float64(ourTotal) / float64(rawTotal)
	vsFFmpeg := float64(ourTotal) / float64(theirTotal)
	t.Logf("ours %d bytes, ffmpeg %d, raw %d; %.3f of raw, %.3f of ffmpeg",
		ourTotal, theirTotal, rawTotal, vsRaw, vsFFmpeg)

	if vsRaw > 0.60 {
		t.Errorf("compressed to %.1f%% of raw, want under 60%%", 100*vsRaw)
	}
	// The encoder uses fixed predictors only, so some headroom against ffmpeg's LPC is expected.
	if vsFFmpeg > 1.15 {
		t.Errorf("output is %.2fx ffmpeg's size, want at most 1.15x", vsFFmpeg)
	}
}
