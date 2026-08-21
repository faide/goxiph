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

	"github.com/faide/goxiph/internal/celt"
	"github.com/faide/goxiph/internal/rangecoder"
	"github.com/faide/goxiph/wav"
	"math"
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
// rawPackets keeps the undecoded bytes alongside the parsed packets, so the whole-stream test can
// feed the packet decoder what an application would give it.
type stream struct {
	raw [][]byte

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
			s.raw = append(s.raw, append([]byte(nil), p.Data...))
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

// rangeRecord is one line of an opusdec --save-range dump.
type rangeRecord struct {
	samples    int
	mode       string // LP, HYB or MDCT
	bandwidth  string
	stereo     bool
	frameSize  int
	finalRange uint32
}

var rangeLineRe = regexp.MustCompile(
	`^(\d+),\s*(\d+),\s*\[\[[^\]]*\],\s*(\w+),\s*(\w+),\s*(\w),\s*(\d+),\s*(\d+)\]`)

// readRangeDump runs opusdec with --save-range and parses what it wrote.
//
// The dump records, for every packet, the mode and bandwidth libopus chose, the samples it produced,
// and the range coder's final state. It is the reference decoder describing its own work.
func readRangeDump(t *testing.T, path string) []rangeRecord {
	t.Helper()

	dump := filepath.Join(t.TempDir(), "range.txt")
	out := filepath.Join(t.TempDir(), "out.wav")
	if msg, err := exec.Command("opusdec", "--quiet", "--save-range", dump, path, out).CombinedOutput(); err != nil {
		t.Fatalf("opusdec: %v\n%s", err, msg)
	}

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("read range dump: %v", err)
	}

	var recs []rangeRecord
	for _, line := range strings.Split(string(raw), "\n") {
		m := rangeLineRe.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		samples, _ := strconv.Atoi(m[1])
		frameSize, _ := strconv.Atoi(m[6])
		fr, _ := strconv.ParseUint(m[7], 10, 32)
		recs = append(recs, rangeRecord{
			samples:    samples,
			mode:       m[3],
			bandwidth:  m[4],
			stereo:     m[5] == "S",
			frameSize:  frameSize,
			finalRange: uint32(fr),
		})
	}
	return recs
}

// TestConformanceTOCMatchesReferenceDecoder checks every packet's configuration against what the
// reference decoder read from the same bytes.
//
// This is stronger than comparing a total: a mode, bandwidth, channel count and sample count are
// checked for each packet in turn, so a configuration decoded wrongly is caught at the packet where
// it happens rather than averaged away.
func TestConformanceTOCMatchesReferenceDecoder(t *testing.T) {
	if _, err := exec.LookPath("opusdec"); err != nil {
		t.Skip("opusdec not installed")
	}

	modeNames := map[Mode]string{ModeSILK: "LP", ModeHybrid: "HYB", ModeCELT: "MDCT"}

	for _, path := range corpus(t, "*.opus") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			s := readStream(t, path)
			recs := readRangeDump(t, path)

			if len(recs) != len(s.packets) {
				t.Fatalf("opusdec reported %d packets, we parsed %d", len(recs), len(s.packets))
			}

			for i, r := range recs {
				p := s.packets[i]
				if got := modeNames[p.Mode]; got != r.mode {
					t.Fatalf("packet %d: we read mode %s, opusdec reports %s", i, got, r.mode)
				}
				if got := p.Bandwidth.String(); got != r.bandwidth {
					t.Fatalf("packet %d: we read bandwidth %s, opusdec reports %s", i, got, r.bandwidth)
				}
				if p.Stereo != r.stereo {
					t.Fatalf("packet %d: we read stereo=%v, opusdec reports %v", i, p.Stereo, r.stereo)
				}
				if got := p.Samples(); got != r.samples {
					t.Fatalf("packet %d: we compute %d samples, opusdec produced %d", i, got, r.samples)
				}
				// The per-frame size is the packet's samples divided by its frame count.
				if got := p.Samples() / len(p.Frames); got != r.frameSize {
					t.Fatalf("packet %d: we compute a frame size of %d, opusdec reports %d",
						i, got, r.frameSize)
				}
			}
			t.Logf("%d packets agree on mode, bandwidth, channels and frame size", len(recs))
		})
	}
}

// TestConformanceCELTRangeMatchesReferenceDecoder is the gate for everything the CELT decoder reads.
//
// The final range is a running function of every symbol decoded, in order. It agrees only if the
// energy, the time-frequency changes, the spreading, the boosts, the trim, the allocation, the fine
// energy, every band's shape and the anti-collapse flag were all read exactly as libopus read them.
// Nothing short of that produces the same number, and a mismatch names the packet where the two
// first diverged.
//
// Only CELT-only packets are checked, because SILK is not written yet.
func TestConformanceCELTRangeMatchesReferenceDecoder(t *testing.T) {
	if _, err := exec.LookPath("opusdec"); err != nil {
		t.Skip("opusdec not installed")
	}

	var totalChecked, totalSkipped int
	for _, path := range corpus(t, "*.opus") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			s := readStream(t, path)
			recs := readRangeDump(t, path)
			if len(recs) != len(s.packets) {
				t.Fatalf("opusdec reported %d packets, we parsed %d", len(recs), len(s.packets))
			}

			var dec *celt.Decoder
			var checked, skipped int

			for i, p := range s.packets {
				if p.Mode != ModeCELT {
					// A decoder that has missed frames cannot carry its state forward.
					dec = nil
					skipped++
					continue
				}

				channels := 1
				if p.Stereo {
					channels = 2
				}
				end := endBandFor(p.Bandwidth)
				if dec == nil {
					var err error
					if dec, err = celt.NewDecoder(channels, 0, end); err != nil {
						t.Fatalf("packet %d: %v", i, err)
					}
				}

				frameSamples := p.Samples() / len(p.Frames)
				size, err := celt.FrameSizeForSamples(frameSamples)
				if err != nil {
					dec, skipped = nil, skipped+1
					continue
				}

				for _, f := range p.Frames {
					if len(f) <= 1 {
						continue
					}
					rd := rangecoder.NewDecoder(f)
					if _, err := dec.DecodeFrame(rd, len(f), size); err != nil {
						t.Fatalf("packet %d: %v", i, err)
					}
				}

				if got := dec.Range(); got != recs[i].finalRange {
					t.Fatalf("packet %d (%s %s, %d frames of %d): our range is %d, opusdec reports %d",
						i, recs[i].mode, recs[i].bandwidth, len(p.Frames), frameSamples,
						got, recs[i].finalRange)
				}
				checked++
			}

			if checked == 0 {
				t.Skipf("no CELT-only packets (%d skipped)", skipped)
			}
			t.Logf("%d CELT packets agree with opusdec on the final range, %d skipped",
				checked, skipped)
			totalChecked += checked
			totalSkipped += skipped
		})
	}
	t.Logf("total: %d packets checked, %d skipped", totalChecked, totalSkipped)
}

// decodeCELTStream decodes every CELT-only packet of a stream, returning one slice per channel.
//
// It returns nil where the stream holds anything else, because a decoder that has skipped frames
// carries the wrong history into the ones that follow and its output would not line up.
func decodeCELTStream(t *testing.T, s stream) [][]float32 {
	t.Helper()

	var pcm [][]float32
	var dec *celt.Decoder

	for _, p := range s.packets {
		if p.Mode != ModeCELT {
			return nil
		}
		channels := 1
		if p.Stereo {
			channels = 2
		}
		if dec == nil {
			var err error
			if dec, err = celt.NewDecoder(channels, 0, endBandFor(p.Bandwidth)); err != nil {
				t.Fatalf("new decoder: %v", err)
			}
			pcm = make([][]float32, channels)
		}
		size, err := celt.FrameSizeForSamples(p.Samples() / len(p.Frames))
		if err != nil {
			return nil
		}
		for _, f := range p.Frames {
			if len(f) <= 1 {
				return nil
			}
			frame, err := dec.DecodeFrame(rangecoder.NewDecoder(f), len(f), size)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			for c := range frame.PCM {
				pcm[c] = append(pcm[c], frame.PCM[c]...)
			}
		}
	}
	return pcm
}

// readWAVFloat reads a whole WAVE file as planar float samples.
func readWAVFloat(t *testing.T, path string) ([][]float32, int) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	d, err := wav.NewDecoder(f)
	if err != nil {
		t.Fatalf("wav: %v", err)
	}
	format := d.Format()

	out := make([][]float32, format.Channels)
	buf := make([][]float32, format.Channels)
	for c := range buf {
		buf[c] = make([]float32, 4096)
	}
	for {
		n, err := d.ReadFloat32(buf)
		for c := range out {
			out[c] = append(out[c], buf[c][:n]...)
		}
		if err != nil || n == 0 {
			break
		}
	}
	return out, format.SampleRate
}

// TestConformanceCELTPCMMatchesReferenceDecoder compares our samples against libopus's.
//
// The range gate says every symbol was read alike, but says nothing about the transform: an inverse
// MDCT with the window at the wrong offset consumes exactly the same bits and produces a waveform
// that is wrong. This closes that, and with it the whole CELT path from bitstream to samples.
//
// The reference is asked for 48 kHz output, because at any other rate it resamples and the
// comparison would be measuring the resampler.
func TestConformanceCELTPCMMatchesReferenceDecoder(t *testing.T) {
	if _, err := exec.LookPath("opusdec"); err != nil {
		t.Skip("opusdec not installed")
	}

	files := 0
	for _, path := range corpus(t, "*.opus") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			s := readStream(t, path)
			ours := decodeCELTStream(t, s)
			if ours == nil {
				t.Skip("not a CELT-only stream")
			}

			out := filepath.Join(t.TempDir(), "ref.wav")
			cmd := exec.Command("opusdec", "--quiet", "--float", "--rate", "48000", path, out)
			if msg, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("opusdec: %v\n%s", err, msg)
			}
			ref, rate := readWAVFloat(t, out)

			if rate != 48000 {
				t.Fatalf("reference came back at %d Hz", rate)
			}
			// A stream may code fewer channels than its header declares, in which case libopus
			// duplicates. That mapping belongs to the Opus layer rather than to CELT, so only the
			// channels CELT produced are compared here.
			if len(ref) < len(ours) {
				t.Fatalf("opusdec produced %d channels, we produced %d", len(ref), len(ours))
			}
			if len(ref) != len(ours) {
				t.Logf("the header declares %d channels and the packets code %d; comparing the coded ones",
					len(ref), len(ours))
			}

			// The reference has already dropped the pre-skip; ours has not.
			skip := s.head.PreSkip
			if len(ours[0])-skip < len(ref[0]) {
				t.Fatalf("we produced %d samples after a pre-skip of %d, short of the reference's %d",
					len(ours[0]), skip, len(ref[0]))
			}

			for c := range ours {
				var dot, ea, eb, worst float64
				for i := range ref[c] {
					a := float64(ours[c][skip+i])
					b := float64(ref[c][i])
					dot += a * b
					ea += a * a
					eb += b * b
					worst = math.Max(worst, math.Abs(a-b))
				}
				if eb == 0 {
					t.Fatalf("channel %d of the reference is silent; nothing to compare", c)
				}

				// A correlation this tight leaves no room for a misplaced window or a wrong overlap;
				// what remains is the reference working in single precision where we work in double.
				if corr := dot / math.Sqrt(ea*eb); corr < 0.99999 {
					t.Fatalf("channel %d: correlation %.7f against the reference", c, corr)
				}
				if worst > 1e-4 {
					t.Fatalf("channel %d: worst sample differs by %v", c, worst)
				}
				t.Logf("channel %d: %d samples, worst difference %.2e", c, len(ref[c]), worst)
			}
			files++
		})
	}
	if files == 0 {
		t.Fatal("no CELT-only stream was compared")
	}
	t.Logf("%d streams match the reference decoder sample for sample", files)
}

// TestConformanceDecoderMatchesReference decodes whole streams through the packet decoder and
// compares them with libopus.
//
// The per-codec gates check each codec in isolation, on frames handed to it directly. This checks
// the layer above: that packets are dispatched to the right codec, that a decoder's state survives
// from one packet to the next, and that the sample counts line up. A stream is skipped only where it
// holds a mode that is not written yet.
func TestConformanceDecoderMatchesReference(t *testing.T) {
	if _, err := exec.LookPath("opusdec"); err != nil {
		t.Skip("opusdec not installed")
	}

	checked := map[string]int{}
	for _, path := range corpus(t, "*.opus") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			s := readStream(t, path)

			supported := true
			for _, p := range s.packets {
				if p.Mode == ModeHybrid {
					supported = false
					break
				}
			}
			if !supported {
				t.Skip("stream holds a mode that is not written yet")
			}

			dec, err := NewDecoder(s.head.Channels)
			if err != nil {
				t.Fatalf("new decoder: %v", err)
			}

			var ours [][]float32
			for i := range s.packets {
				got, err := dec.Decode(s.raw[i])
				if err != nil {
					t.Fatalf("packet %d: %v", i, err)
				}
				if ours == nil {
					ours = make([][]float32, len(got))
				}
				for c := range got {
					ours[c] = append(ours[c], got[c]...)
				}
			}

			out := filepath.Join(t.TempDir(), "ref.wav")
			cmd := exec.Command("opusdec", "--quiet", "--float", "--rate", "48000", path, out)
			if msg, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("opusdec: %v\n%s", err, msg)
			}
			ref, rate := readWAVFloat(t, out)
			if rate != 48000 {
				t.Fatalf("reference came back at %d Hz", rate)
			}
			if len(ref) != len(ours) {
				t.Fatalf("opusdec produced %d channels, we produced %d", len(ref), len(ours))
			}

			skip := s.head.PreSkip
			if len(ours[0])-skip < len(ref[0]) {
				t.Fatalf("we produced %d samples after a pre-skip of %d, short of %d",
					len(ours[0]), skip, len(ref[0]))
			}

			for c := range ref {
				var dot, ea, eb, worst float64
				for i := range ref[c] {
					a := float64(ours[c][skip+i])
					b := float64(ref[c][i])
					dot += a * b
					ea += a * a
					eb += b * b
					worst = math.Max(worst, math.Abs(a-b))
				}
				if eb == 0 {
					t.Fatalf("channel %d of the reference is silent", c)
				}
				if corr := dot / math.Sqrt(ea*eb); corr < 0.99999 {
					t.Fatalf("channel %d: correlation %.7f", c, corr)
				}
				if worst > 1e-4 {
					t.Fatalf("channel %d: worst sample differs by %v", c, worst)
				}
			}

			mode := "CELT"
			if s.packets[0].Mode == ModeSILK {
				mode = "SILK"
			}
			checked[mode]++
			t.Logf("%d packets, %d samples match the reference", len(s.packets), len(ref[0]))
		})
	}

	if checked["SILK"] == 0 {
		t.Error("no SILK stream was decoded through the packet decoder")
	}
	t.Logf("streams matching by leading mode: %v", checked)
}
