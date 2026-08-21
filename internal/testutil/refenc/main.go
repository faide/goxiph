// Command refenc builds Opus fixtures that opusenc will not produce.
//
// opusenc picks its own channel count and mode, and there is no setting that makes it emit a stereo
// SILK packet: below about fourteen kilobits it downmixes to mono, and above it moves to hybrid.
// The reference encoder can be told directly, through a control the public API does not expose, so
// gen_stereo.c drives it and this wraps what comes out in an Ogg stream.
//
// It needs the extracted reference and a C compiler, so it is not part of the normal fixture run.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/faide/goxiph/ogg"
	"github.com/faide/goxiph/opus"
)

const pi = math.Pi

func sin(x float64) float64 { return math.Sin(x) }

const (
	sampleRate  = 48000
	frameSize   = 960 // 20 ms
	preSkip     = 312
	serialplace = 0x5C1E0001
)

func main() {
	spec := flag.String("spec", ".specs/opus-rfc6716", "extracted reference implementation")
	out := flag.String("out", "testdata/generated", "output directory")
	flag.Parse()

	if err := run(*spec, *out); err != nil {
		fmt.Fprintln(os.Stderr, "refenc:", err)
		os.Exit(1)
	}
}

// The reference's internal mode numbers, from src/opus_private.h. They are not part of the public
// interface, which is why forcing a mode needs the private header.
const (
	modeSILKOnly = 1000
	modeHybrid   = 1001
	modeCELTOnly = 1002
)

type variant struct {
	name    string
	bitrate int
	mode    int
	comment string
}

func run(spec, out string) error {
	if _, err := os.Stat(filepath.Join(spec, "src", "opus_encoder.c")); err != nil {
		return fmt.Errorf("reference not extracted in %s; run `mise run specs:fetch`", spec)
	}

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "refenc")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	binary := filepath.Join(tmp, "gen_stereo")
	if err := build(spec, binary); err != nil {
		return err
	}

	raw := filepath.Join(tmp, "stereo.raw")
	if err := os.WriteFile(raw, stereoSignal(2*sampleRate), 0o644); err != nil {
		return err
	}

	variants := []variant{
		{"opus_silk_stereo", 16000, modeSILKOnly, "SILK stereo, which opusenc will not produce at any rate"},
		{"opus_hybrid_stereo", 32000, modeHybrid, "hybrid stereo, where both codecs read one packet"},
	}
	for _, v := range variants {
		packets := filepath.Join(tmp, v.name+".pkts")
		cmd := exec.Command(binary, raw, packets, fmt.Sprint(v.bitrate), fmt.Sprint(v.mode))
		if msg, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w\n%s", v.name, err, msg)
		}
		if err := wrap(packets, filepath.Join(out, v.name+".opus")); err != nil {
			return fmt.Errorf("%s: %w", v.name, err)
		}
		fmt.Printf("refenc: wrote %s.opus (%s)\n", v.name, v.comment)
	}
	return nil
}

func build(spec, binary string) error {
	// The C source sits under testdata so that the Go toolchain does not try to build it as cgo,
	// which fails outright once CGO_ENABLED is set.
	sources := []string{filepath.Join("internal", "testutil", "refenc", "testdata", "gen_stereo.c")}
	abs, err := filepath.Abs(sources[0])
	if err != nil {
		return err
	}

	// The demo files carry their own main, so they are left out.
	var celt []string
	matches, _ := filepath.Glob(filepath.Join(spec, "celt", "*.c"))
	for _, m := range matches {
		if !filepath.IsAbs(m) && filepath.Base(m) == "opus_custom_demo.c" {
			continue
		}
		celt = append(celt, m)
	}
	silk, _ := filepath.Glob(filepath.Join(spec, "silk", "*.c"))
	silkFloat, _ := filepath.Glob(filepath.Join(spec, "silk", "float", "*.c"))

	args := []string{
		"-O2", "-DVAR_ARRAYS", "-DOPUS_BUILD", "-w",
		"-I", filepath.Join(spec, "celt"),
		"-I", filepath.Join(spec, "silk"),
		"-I", filepath.Join(spec, "silk", "float"),
		"-I", filepath.Join(spec, "include"),
		"-I", filepath.Join(spec, "src"),
		"-o", binary, abs,
	}
	args = append(args, celt...)
	args = append(args, silk...)
	args = append(args, silkFloat...)
	for _, f := range []string{"opus.c", "opus_encoder.c", "opus_decoder.c", "repacketizer.c"} {
		args = append(args, filepath.Join(spec, "src", f))
	}
	args = append(args, "-lm")

	if msg, err := exec.Command("gcc", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("building the reference encoder: %w\n%s", err, msg)
	}
	return nil
}

// stereoSignal makes a signal whose two channels differ, so that mid and side both carry something.
// Two identical channels would leave the side at zero and the prediction untested.
func stereoSignal(frames int) []byte {
	out := make([]byte, 0, frames*4)
	for i := range frames {
		t := float64(i) / sampleRate
		left := 0.5*sin(2*pi*440*t) + 0.2*sin(2*pi*1300*t)
		right := 0.5*sin(2*pi*440*t+0.9) + 0.2*sin(2*pi*900*t)
		// A rising then wavering envelope, so the gains and the prediction both move.
		env := min(1.0, t*3) * (0.6 + 0.4*sin(2*pi*2*t))

		var sample [4]byte
		binary.LittleEndian.PutUint16(sample[0:], uint16(int16(28000*env*left)))
		binary.LittleEndian.PutUint16(sample[2:], uint16(int16(28000*env*right)))
		out = append(out, sample[:]...)
	}
	return out
}

// wrap turns a length-prefixed packet stream into an Ogg Opus file.
func wrap(packets, path string) error {
	data, err := os.ReadFile(packets)
	if err != nil {
		return err
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	m := ogg.NewMuxer(f, serialplace)
	head := opus.Head{Version: 1, Channels: 2, PreSkip: preSkip, InputRate: sampleRate}
	headBytes, err := head.AppendTo(nil)
	if err != nil {
		return err
	}
	if err := m.WritePacket(headBytes, 0); err != nil {
		return err
	}
	if err := m.Flush(); err != nil {
		return err
	}
	if err := m.WritePacket(comments(), 0); err != nil {
		return err
	}
	if err := m.Flush(); err != nil {
		return err
	}

	// The granule position counts samples encoded, not samples played: a player subtracts the
	// pre-skip itself. Adding it here would claim more samples than the packets carry.
	granule := int64(0)
	for i := 0; i+2 <= len(data); {
		n := int(data[i])<<8 | int(data[i+1])
		i += 2
		if i+n > len(data) {
			return fmt.Errorf("truncated packet stream")
		}
		granule += frameSize
		if err := m.WritePacket(data[i:i+n], granule); err != nil {
			return err
		}
		i += n
	}
	return m.Close()
}

// comments is the smallest valid OpusTags packet.
func comments() []byte {
	vendor := "goxiph refenc"
	out := append([]byte(nil), "OpusTags"...)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(vendor)))
	out = append(out, vendor...)
	return binary.LittleEndian.AppendUint32(out, 0)
}
