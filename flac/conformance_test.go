//go:build conformance

package flac

import (
	"encoding/binary"
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
