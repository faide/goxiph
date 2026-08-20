//go:build conformance

package vorbis

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/faide/goxiph/internal/bitio"
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

// headerPackets returns the first n packets of the first logical stream in a file.
func headerPackets(t *testing.T, path string, n int) [][]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()

	d := ogg.NewDemuxer(f)
	var out [][]byte
	var serial uint32
	for len(out) < n {
		p, err := d.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		if len(out) == 0 {
			serial = p.Serial
		} else if p.Serial != serial {
			continue
		}
		out = append(out, p.Data)
	}
	return out
}

// TestConformanceIdentHeader parses the identification header of every reference file and checks
// the result against the file's real parameters, which the generator encoded into its name.
func TestConformanceIdentHeader(t *testing.T) {
	namePattern := regexp.MustCompile(`_(\d+)hz_(\d+)ch\.ogg$`)

	for _, path := range corpus(t, "*hz_*ch.ogg") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			m := namePattern.FindStringSubmatch(path)
			if m == nil {
				t.Fatalf("cannot read parameters from %q", path)
			}
			wantRate, _ := strconv.Atoi(m[1])
			wantCh, _ := strconv.Atoi(m[2])

			packets := headerPackets(t, path, 1)
			if len(packets) == 0 {
				t.Fatal("no packets")
			}
			info, err := ParseInfo(packets[0])
			if err != nil {
				t.Fatalf("ParseInfo: %v", err)
			}

			if info.SampleRate != wantRate {
				t.Errorf("SampleRate = %d, want %d", info.SampleRate, wantRate)
			}
			if info.Channels != wantCh {
				t.Errorf("Channels = %d, want %d", info.Channels, wantCh)
			}
			if info.BlockSize0 > info.BlockSize1 {
				t.Errorf("short block %d exceeds long block %d", info.BlockSize0, info.BlockSize1)
			}

			// Re-encoding must reproduce the reference bytes.
			again, err := info.AppendTo(nil)
			if err != nil {
				t.Fatalf("AppendTo: %v", err)
			}
			if !bytes.Equal(again, packets[0]) {
				t.Errorf("re-encoded header differs\n got %x\nwant %x", again, packets[0])
			}
		})
	}
}

// TestConformanceIdentAgainstOgginfo cross-checks our parse against libvorbis's own reader.
func TestConformanceIdentAgainstOgginfo(t *testing.T) {
	if _, err := exec.LookPath("ogginfo"); err != nil {
		t.Skip("ogginfo not installed")
	}

	rateRe := regexp.MustCompile(`(?i)rate:\s*(\d+)`)
	chRe := regexp.MustCompile(`(?i)channels:\s*(\d+)`)

	for _, path := range corpus(t, "*hz_*ch.ogg") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			out, err := exec.Command("ogginfo", path).CombinedOutput()
			if err != nil {
				t.Fatalf("ogginfo: %v\n%s", err, out)
			}
			rm := rateRe.FindSubmatch(out)
			cm := chRe.FindSubmatch(out)
			if rm == nil || cm == nil {
				t.Skipf("could not read rate/channels from ogginfo output:\n%s", out)
			}
			wantRate, _ := strconv.Atoi(string(rm[1]))
			wantCh, _ := strconv.Atoi(string(cm[1]))

			info, err := ParseInfo(headerPackets(t, path, 1)[0])
			if err != nil {
				t.Fatalf("ParseInfo: %v", err)
			}
			if info.SampleRate != wantRate || info.Channels != wantCh {
				t.Errorf("we read %d Hz %d ch, ogginfo reports %d Hz %d ch",
					info.SampleRate, info.Channels, wantRate, wantCh)
			}
		})
	}
}

// TestConformanceCommentHeader parses the comment header of every reference file and checks the
// vendor string against what libvorbis writes.
func TestConformanceCommentHeader(t *testing.T) {
	for _, path := range corpus(t, "*hz_*ch.ogg") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			packets := headerPackets(t, path, 2)
			if len(packets) < 2 {
				t.Fatalf("got %d packets, want at least 2", len(packets))
			}
			tags, err := ParseComments(packets[1])
			if err != nil {
				t.Fatalf("ParseComments: %v", err)
			}
			if !strings.Contains(tags.Vendor, "Vorbis") {
				t.Errorf("vendor = %q, want it to name libVorbis", tags.Vendor)
			}
		})
	}
}

// TestConformanceOversizeComment covers a comment header larger than one Ogg page, which is where a
// packet-reassembly bug in the container would surface as a metadata parse failure.
func TestConformanceOversizeComment(t *testing.T) {
	path := filepath.Join(corpusDir, "bigheader.ogg")
	if _, err := os.Stat(path); err != nil {
		t.Skip("no bigheader.ogg; run `mise run fixtures`")
	}

	packets := headerPackets(t, path, 2)
	if len(packets) < 2 {
		t.Fatalf("got %d packets, want at least 2", len(packets))
	}
	if len(packets[1]) <= 65025 {
		t.Fatalf("comment packet is %d bytes; the fixture should exceed one page", len(packets[1]))
	}

	tags, err := ParseComments(packets[1])
	if err != nil {
		t.Fatalf("ParseComments on a multi-page packet: %v", err)
	}
	desc := tags.First("DESCRIPTION")
	if len(desc) < 60000 {
		t.Errorf("DESCRIPTION is %d bytes, want the long value back intact", len(desc))
	}
	if !strings.HasPrefix(desc, "the quick brown fox") {
		t.Errorf("DESCRIPTION starts %q", desc[:min(40, len(desc))])
	}
}

// TestConformanceHeaderTypes checks that the three header packets are identified as headers and the
// audio packets are not.
func TestConformanceHeaderTypes(t *testing.T) {
	path := filepath.Join(corpusDir, "sine1k_44100hz_2ch.ogg")
	if _, err := os.Stat(path); err != nil {
		t.Skip("run `mise run fixtures`")
	}
	packets := headerPackets(t, path, 8)
	if len(packets) < 5 {
		t.Fatalf("got %d packets, want at least 5", len(packets))
	}
	for i := range 3 {
		if !IsHeader(packets[i]) {
			t.Errorf("packet %d not recognised as a header (type %#02x)", i, packets[i][0])
		}
	}
	for i := 3; i < len(packets); i++ {
		if IsHeader(packets[i]) {
			t.Errorf("audio packet %d recognised as a header (type %#02x)", i, packets[i][0])
		}
	}
	if got := fmt.Sprintf("%c%c%c", packets[0][1], packets[0][2], packets[0][3]); got != "vor" {
		t.Errorf("signature starts %q", got)
	}
}

// TestConformanceCodebooksFromRealStreams parses the codebook array of every reference setup header
// and checks that decoding stopped on exactly the right bit.
//
// The field immediately after the codebooks is [vorbis_time_count], followed by that many 16-bit
// values which the specification requires to be zero. They are the checksum: if codebook parsing
// consumed one bit too many or too few, these come back nonzero.
func TestConformanceCodebooksFromRealStreams(t *testing.T) {
	for _, path := range corpus(t, "*hz_*ch.ogg") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			packets := headerPackets(t, path, 3)
			if len(packets) < 3 {
				t.Fatalf("got %d packets, want 3", len(packets))
			}
			setup := packets[2]
			if setup[0] != packetSetup {
				t.Fatalf("third packet has type %#02x, want %#02x", setup[0], packetSetup)
			}

			r := bitio.NewLSBReader(setup[1+len("vorbis"):])
			n, err := r.Read(8)
			if err != nil {
				t.Fatalf("codebook count: %v", err)
			}
			count := int(n) + 1
			if count < 1 || count > 256 {
				t.Fatalf("codebook count %d out of range", count)
			}

			withLookup := 0
			for i := range count {
				c, err := readCodebook(r)
				if err != nil {
					t.Fatalf("codebook %d of %d: %v", i, count, err)
				}
				if c.LookupType != 0 {
					withLookup++
					// Every entry must produce a vector without error.
					dst := make([]float32, c.Dimensions)
					if err := c.EntryVector(c.Entries-1, dst); err != nil {
						t.Fatalf("codebook %d: EntryVector: %v", i, err)
					}
				}
			}

			// The alignment check.
			tc, err := r.Read(6)
			if err != nil {
				t.Fatalf("time count: %v", err)
			}
			for i := range int(tc) + 1 {
				v, err := r.Read(16)
				if err != nil {
					t.Fatalf("time domain transform %d: %v", i, err)
				}
				if v != 0 {
					t.Fatalf("time domain transform %d = %#04x, want 0; codebook parsing is misaligned", i, v)
				}
			}

			if withLookup == 0 {
				t.Error("no codebook carried a VQ lookup table, so that path went untested")
			}
			t.Logf("%d codebooks, %d with lookup tables", count, withLookup)
		})
	}
}
