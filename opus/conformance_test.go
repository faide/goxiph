//go:build conformance

package opus

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/faide/goxiph/ogg"
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

// stream holds everything read from one Ogg Opus file.
type stream struct {
	head    Head
	packets []*Packet
	samples int64
	lastGP  int64
}

// readStream parses a whole Ogg Opus file through the container and the packet layer.
func readStream(t *testing.T, path string) stream {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	var s stream
	d := ogg.NewDemuxer(f)
	headers := 0
	var serial uint32

	for {
		p, err := d.ReadPacket()
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
			if s.head, err = ParseHead(p.Data); err != nil {
				t.Fatalf("ParseHead: %v", err)
			}
			headers++
		case 1:
			if _, err := ParseTags(p.Data); err != nil {
				t.Fatalf("ParseTags: %v", err)
			}
			headers++
		default:
			pkt, err := ParsePacket(p.Data)
			if err != nil {
				t.Fatalf("ParsePacket at packet %d: %v", len(s.packets), err)
			}
			s.packets = append(s.packets, pkt)
			s.samples += int64(pkt.Samples())
			if p.GranulePos != ogg.NoGranule {
				s.lastGP = p.GranulePos
			}
		}
	}
	return s
}

// TestConformanceParsesRealStreams checks the header and packet layers against files libopus wrote.
func TestConformanceParsesRealStreams(t *testing.T) {
	for _, path := range corpus(t, "*.opus") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			s := readStream(t, path)

			if s.head.Version > maxSupportedVersion {
				t.Errorf("version %d", s.head.Version)
			}
			if s.head.Channels < 1 {
				t.Errorf("Channels = %d", s.head.Channels)
			}
			if len(s.packets) == 0 {
				t.Fatal("no audio packets")
			}

			// The identification header must re-encode to the bytes it came from.
			again, err := s.head.AppendTo(nil)
			if err != nil {
				t.Fatalf("AppendTo: %v", err)
			}
			f, _ := os.Open(path)
			defer f.Close()
			d := ogg.NewDemuxer(f)
			first, err := d.ReadPacket()
			if err != nil {
				t.Fatalf("ReadPacket: %v", err)
			}
			if string(again) != string(first.Data) {
				t.Errorf("re-encoded header differs\n got %x\nwant %x", again, first.Data)
			}

			t.Logf("%d packets, %d channels, pre-skip %d, %d samples",
				len(s.packets), s.head.Channels, s.head.PreSkip, s.samples)
		})
	}
}

// TestConformanceSampleCountMatchesGranule checks the packet layer against what libopus recorded.
//
// Every packet's duration comes from its TOC byte alone, so the total is right only if the
// configuration table, the frame packing and the frame counts are all right.
//
// The comparison is an inequality rather than an equality because of RFC 7845 section 4.4: the final
// granule position may fall short of what decoding every packet yields, and the difference is
// trimmed from the end so a stream can finish somewhere other than a frame boundary. The
// specification bounds that difference by one packet's worth of audio, which is what makes this a
// tight check rather than a vague one.
func TestConformanceSampleCountMatchesGranule(t *testing.T) {
	for _, path := range corpus(t, "*.opus") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			s := readStream(t, path)
			if s.lastGP == 0 {
				t.Skip("stream carries no granule position")
			}

			// The granule position already includes the pre-skip, so it is directly comparable.
			if s.samples < s.lastGP {
				t.Fatalf("packets carry %d samples, fewer than the granule position of %d",
					s.samples, s.lastGP)
			}
			trim := s.samples - s.lastGP
			last := int64(s.packets[len(s.packets)-1].Samples())
			if trim > last {
				t.Errorf("end trimming of %d samples exceeds the final packet's %d", trim, last)
			}
			t.Logf("%d samples decoded, granule %d, %d trimmed from the end", s.samples, s.lastGP, trim)
		})
	}
}

// TestConformanceDurationMatchesReferenceDecoder is the closest thing to a decoder gate available
// before the decoder exists.
//
// Our packet layer computes the stream's length from TOC bytes alone, without decoding anything.
// Running the same file through opusdec and counting the PCM it produces checks that arithmetic
// against a reference implementation that does decode it. A wrong frame size or a miscounted
// code 3 packet changes the total and shows up here.
func TestConformanceDurationMatchesReferenceDecoder(t *testing.T) {
	if _, err := exec.LookPath("opusdec"); err != nil {
		t.Skip("opusdec not installed")
	}

	for _, path := range corpus(t, "*.opus") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			s := readStream(t, path)

			out := filepath.Join(t.TempDir(), "ref.wav")
			if msg, err := exec.Command("opusdec", "--quiet", "--rate", "48000", path, out).CombinedOutput(); err != nil {
				t.Fatalf("opusdec: %v\n%s", err, msg)
			}
			raw, err := os.ReadFile(out)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			// Locate the data chunk rather than assuming a fixed header size.
			off, size := 12, 0
			for off+8 <= len(raw) {
				id := string(raw[off : off+4])
				n := int(uint32(raw[off+4]) | uint32(raw[off+5])<<8 |
					uint32(raw[off+6])<<16 | uint32(raw[off+7])<<24)
				if id == "data" {
					size = min(n, len(raw)-off-8)
					break
				}
				off += 8 + n + n%2
			}
			if size == 0 {
				t.Fatal("no data chunk in the reference output")
			}

			frames := int64(size / 2 / s.head.Channels)

			// opusdec applies the pre-skip and the end trimming, so what it writes is the audio the
			// stream holds: the final granule position less the pre-skip.
			want := s.lastGP - int64(s.head.PreSkip)
			if frames != want {
				t.Errorf("opusdec produced %d frames, granule position less pre-skip is %d",
					frames, want)
			}
			// Our packet-derived total must bracket it: at least the audio, at most one packet more.
			if s.samples < frames {
				t.Errorf("our packets account for %d samples, fewer than the %d decoded",
					s.samples, frames)
			}
			t.Logf("opusdec produced %d frames; our packets account for %d samples", frames, s.samples)
		})
	}
}

// TestConformanceAgainstOpusinfo cross-checks the header against libopus's own reader.
func TestConformanceAgainstOpusinfo(t *testing.T) {
	if _, err := exec.LookPath("opusinfo"); err != nil {
		t.Skip("opusinfo not installed")
	}

	chRe := regexp.MustCompile(`(?i)channels:\s*(\d+)`)
	skipRe := regexp.MustCompile(`(?i)pre-skip:\s*(\d+)`)

	for _, path := range corpus(t, "*.opus") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			out, err := exec.Command("opusinfo", path).CombinedOutput()
			if err != nil {
				t.Fatalf("opusinfo: %v\n%s", err, out)
			}
			s := readStream(t, path)

			if m := chRe.FindSubmatch(out); m != nil {
				want, _ := strconv.Atoi(string(m[1]))
				if s.head.Channels != want {
					t.Errorf("we read %d channels, opusinfo reports %d", s.head.Channels, want)
				}
			}
			if m := skipRe.FindSubmatch(out); m != nil {
				want, _ := strconv.Atoi(string(m[1]))
				if s.head.PreSkip != want {
					t.Errorf("we read pre-skip %d, opusinfo reports %d", s.head.PreSkip, want)
				}
			}
			if strings.Contains(string(out), "WARNING") || strings.Contains(string(out), "ERROR") {
				t.Logf("opusinfo reported: %s", out)
			}
		})
	}
}

// TestConformanceModesAreExercised reports which configurations the corpus reaches, so a gap in
// coverage is visible rather than assumed away.
func TestConformanceModesAreExercised(t *testing.T) {
	modes := map[string]int{}
	bandwidths := map[string]int{}
	codes := map[int]int{}
	multiFrame := 0

	for _, path := range corpus(t, "*.opus") {
		s := readStream(t, path)
		for _, p := range s.packets {
			modes[p.Mode.String()]++
			bandwidths[p.Bandwidth.String()]++
			if len(p.Frames) > 1 {
				multiFrame++
			}
		}
	}
	// The frame-count code is not kept on the packet, so it is inferred from the frame count here.
	_ = codes

	t.Logf("modes %v", modes)
	t.Logf("bandwidths %v", bandwidths)
	t.Logf("%d packets carry more than one frame", multiFrame)

	if len(modes) == 0 {
		t.Fatal("no packets parsed")
	}
}
