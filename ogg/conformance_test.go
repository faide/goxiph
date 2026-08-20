//go:build conformance

package ogg

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const corpusDir = "../testdata/generated"

// corpus lists the generated reference streams, skipping the test when they are absent so a fresh
// clone still passes.
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

func needTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not installed", name)
	}
}

// TestConformancePageRoundTrip is the core container gate: every page libvorbis and libopus produce
// must re-serialize to the exact bytes it came from.
func TestConformancePageRoundTrip(t *testing.T) {
	files := append(corpus(t, "*.ogg"), corpus(t, "*.opus")...)
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			r := NewReader(bytes.NewReader(data))
			var rebuilt []byte
			pages := 0
			for {
				page, err := r.ReadPage()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("page %d: %v", pages, err)
				}
				image, err := page.Marshal()
				if err != nil {
					t.Fatalf("page %d: marshal: %v", pages, err)
				}
				rebuilt = append(rebuilt, image...)
				pages++
			}

			if r.Skipped() != 0 {
				t.Errorf("skipped %d bytes of a well-formed file", r.Skipped())
			}
			if pages == 0 {
				t.Fatal("no pages read")
			}
			if !bytes.Equal(rebuilt, data) {
				t.Errorf("re-serialized stream differs from the original (%d pages, %d vs %d bytes)",
					pages, len(rebuilt), len(data))
			}
		})
	}
}

// TestConformanceRemuxPreservesAudio demuxes a reference file to packets, re-muxes it with our
// writer, and has libvorbis decode both. The container is correct only if the audio is unchanged.
func TestConformanceRemuxPreservesAudio(t *testing.T) {
	needTool(t, "oggdec")
	needTool(t, "ogginfo")

	for _, path := range corpus(t, "*_44100hz_2ch.ogg") {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			remuxed := remux(t, data)

			dir := t.TempDir()
			ours := filepath.Join(dir, "ours.ogg")
			if err := os.WriteFile(ours, remuxed, 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}

			// ogginfo rejects granulepos discontinuities, bad page ordering and broken framing.
			out, err := exec.Command("ogginfo", ours).CombinedOutput()
			if err != nil {
				t.Fatalf("ogginfo rejected our stream: %v\n%s", err, out)
			}
			for _, bad := range []string{"WARNING", "ERROR", "Warning", "Error"} {
				if bytes.Contains(out, []byte(bad)) {
					t.Errorf("ogginfo reported %s:\n%s", bad, out)
					break
				}
			}

			if !bytes.Equal(decodePCM(t, path), decodePCM(t, ours)) {
				t.Error("audio changed across a demux/remux cycle")
			}
		})
	}
}

// TestConformanceChained checks that a physically concatenated pair of streams yields both.
func TestConformanceChained(t *testing.T) {
	path := filepath.Join(corpusDir, "chained.ogg")
	if _, err := os.Stat(path); err != nil {
		t.Skip("no chained.ogg; run `mise run fixtures`")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	d := NewDemuxer(bytes.NewReader(data))
	starts, ends := 0, 0
	for {
		p, err := d.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		if p.FirstPage {
			starts++
		}
		if p.LastPage {
			ends++
		}
	}
	if starts != 2 || ends != 2 {
		t.Errorf("got %d stream starts and %d ends, want 2 and 2", starts, ends)
	}
}

// TestConformanceOversizePacket covers packets that span pages, using a file whose comment header
// exceeds one page.
func TestConformanceOversizePacket(t *testing.T) {
	path := filepath.Join(corpusDir, "bigheader.ogg")
	if _, err := os.Stat(path); err != nil {
		t.Skip("no bigheader.ogg; run `mise run fixtures`")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	d := NewDemuxer(bytes.NewReader(data))
	biggest := 0
	for {
		p, err := d.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}
		if len(p.Data) > biggest {
			biggest = len(p.Data)
		}
	}
	if biggest <= MaxPayload {
		t.Errorf("largest packet is %d bytes; the fixture should exceed one page (%d)", biggest, MaxPayload)
	}
}

// remux runs a stream through the demuxer and back out through the muxer.
func remux(t *testing.T, data []byte) []byte {
	t.Helper()

	type stream struct {
		buf bytes.Buffer
		mux *Muxer
	}
	var order []uint32
	streams := map[uint32]*stream{}

	d := NewDemuxer(bytes.NewReader(data))
	for {
		p, err := d.ReadPacket()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadPacket: %v", err)
		}

		s := streams[p.Serial]
		if s == nil || p.FirstPage {
			s = &stream{}
			s.mux = NewMuxer(&s.buf, p.Serial)
			streams[p.Serial] = s
			order = append(order, p.Serial)
		}
		if err := s.mux.WritePacket(p.Data, p.GranulePos); err != nil {
			t.Fatalf("WritePacket: %v", err)
		}
		// A granule position is page-level metadata: the framing carries only the position of the
		// last packet to end on a page, so the demuxer reports NoGranule for every other packet.
		// Re-muxing therefore has to end a page wherever a position is known, or the rebuilt page
		// would inherit the -1 of whichever packet happened to land last.
		if p.GranulePos != NoGranule {
			if err := s.mux.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
		}
		if p.LastPage {
			if err := s.mux.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		}
	}

	var out []byte
	for _, serial := range order {
		s := streams[serial]
		if err := s.mux.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		out = append(out, s.buf.Bytes()...)
	}
	return out
}

// decodePCM runs a stream through oggdec and returns the raw samples.
func decodePCM(t *testing.T, path string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.wav")
	cmd := exec.Command("oggdec", "-Q", "-o", out, path)
	if msg, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("oggdec %s: %v\n%s", path, err, msg)
	}
	pcm, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read decoded: %v", err)
	}
	return pcm
}
