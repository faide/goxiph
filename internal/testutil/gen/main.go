// Command gen synthesises the conformance corpus.
//
// Test media is generated rather than committed: synthetic signals target specific codec failure
// modes, are exactly reproducible, and carry no copyright.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// A signal is one sox synth recipe.
type signal struct {
	name string
	args []string // sox synth arguments following the duration
}

var signals = []signal{
	{"silence", []string{"sine", "0"}},
	{"sine1k", []string{"sine", "1000"}},
	{"sweep", []string{"sine", "20-20000"}},
	{"noise", []string{"whitenoise"}},
	{"pink", []string{"pinknoise"}},
	{"square", []string{"square", "440"}},
}

// A profile is one sample-rate and channel-count combination.
type profile struct {
	rate     int
	channels int
}

var profiles = []profile{
	{44100, 1},
	{44100, 2},
	{48000, 2},
	{8000, 1},
	{96000, 2},
}

func main() {
	out := flag.String("out", "testdata/generated", "output directory")
	dur := flag.String("duration", "1.5", "signal duration in seconds")
	flag.Parse()

	if err := run(*out, *dur); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(out, dur string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}

	count := 0
	for _, p := range profiles {
		for _, s := range signals {
			base := fmt.Sprintf("%s_%dhz_%dch", s.name, p.rate, p.channels)
			wav := filepath.Join(out, base+".wav")

			args := []string{"-n", "-r", fmt.Sprint(p.rate), "-c", fmt.Sprint(p.channels), "-b", "16", wav, "synth", dur}
			args = append(args, s.args...)
			if err := tool("sox", args...); err != nil {
				return err
			}
			count++

			// A Vorbis stream to exercise the container against real libvorbis framing.
			if err := tool("oggenc", "-Q", "-q", "3", "-o", filepath.Join(out, base+".ogg"), wav); err != nil {
				return err
			}
			count++

			// A FLAC stream. ffmpeg rather than the flac tool: flac and metaflac are GPL while
			// libFLAC is BSD, and ffmpeg is present here anyway.
			if err := tool("ffmpeg", "-v", "error", "-y", "-i", wav,
				"-c:a", "flac", filepath.Join(out, base+".flac")); err != nil {
				return err
			}
			count++
		}
	}

	// FLAC encapsulated in Ogg, which frames each audio packet instead of relying on sync scanning.
	for _, base := range []string{"sine1k_44100hz_2ch", "noise_44100hz_1ch", "sweep_48000hz_2ch"} {
		if err := tool("ffmpeg", "-v", "error", "-y", "-i", filepath.Join(out, base+".wav"),
			"-c:a", "flac", "-f", "ogg", filepath.Join(out, base+".oga")); err != nil {
			return err
		}
		count++
	}

	// FLAC at 24-bit and at both ends of the compression range: level 0 leans on fixed predictors
	// and verbatim subframes, level 12 on high-order LPC.
	src24 := filepath.Join(out, "sweep_44100hz_2ch.wav")
	for _, v := range []struct{ name, depth, level string }{
		{"flac24_level5", "s32", "5"},
		{"flac16_level0", "s16", "0"},
		{"flac16_level12", "s16", "12"},
	} {
		args := []string{"-v", "error", "-y", "-i", src24, "-c:a", "flac",
			"-compression_level", v.level, "-sample_fmt", v.depth,
			filepath.Join(out, v.name+".flac")}
		if err := tool("ffmpeg", args...); err != nil {
			return err
		}
		count++
	}

	// Fixtures that force the subframe types ordinary audio never reaches. Verified by counting:
	// dithered "silence" from sox compresses as LPC, so true digital silence is needed for constant
	// subframes, and incompressible data is needed for verbatim and escaped partitions.
	if err := writeWAV(filepath.Join(out, "truesilence.wav"), 44100, 2, func(int, int) int16 { return 0 }); err != nil {
		return err
	}
	rng := uint32(12345)
	if err := writeWAV(filepath.Join(out, "incompressible.wav"), 44100, 2, func(int, int) int16 {
		// xorshift: uniform bits are what defeats every predictor the format has.
		rng ^= rng << 13
		rng ^= rng >> 17
		rng ^= rng << 5
		return int16(rng)
	}); err != nil {
		return err
	}
	for _, n := range []string{"truesilence", "incompressible"} {
		if err := tool("ffmpeg", "-v", "error", "-y", "-i", filepath.Join(out, n+".wav"),
			"-c:a", "flac", filepath.Join(out, n+".flac")); err != nil {
			return err
		}
		count += 2
	}

	// A packet larger than one page needs a long comment; oggenc carries arbitrary tag text.
	big := filepath.Join(out, "bigheader.ogg")
	long := make([]byte, 0, 70000)
	for len(long) < 70000 {
		long = append(long, "the quick brown fox jumps over the lazy dog "...)
	}
	src := filepath.Join(out, "sine1k_44100hz_2ch.wav")
	if err := tool("oggenc", "-Q", "-q", "1", "-c", "DESCRIPTION="+string(long), "-o", big, src); err != nil {
		return err
	}
	count++

	// An Opus stream, for the second Ogg mapping.
	if err := tool("opusenc", "--quiet", "--bitrate", "64", src, filepath.Join(out, "sine1k.opus")); err != nil {
		return err
	}
	count++

	// A chained file: two logical streams back to back in one physical stream.
	if err := concat(filepath.Join(out, "chained.ogg"),
		filepath.Join(out, "sine1k_44100hz_2ch.ogg"),
		filepath.Join(out, "noise_44100hz_2ch.ogg")); err != nil {
		return err
	}
	count++

	fmt.Printf("gen: wrote %d files to %s\n", count, out)
	return nil
}

// writeWAV emits a 16-bit WAV of one second, sourcing each sample from gen.
func writeWAV(path string, rate, channels int, gen func(frame, ch int) int16) error {
	frames := rate
	data := make([]byte, 0, frames*channels*2)
	for i := range frames {
		for c := range channels {
			v := uint16(gen(i, c))
			data = append(data, byte(v), byte(v>>8))
		}
	}

	var h []byte
	le32 := func(v uint32) { h = append(h, byte(v), byte(v>>8), byte(v>>16), byte(v>>24)) }
	le16 := func(v uint16) { h = append(h, byte(v), byte(v>>8)) }
	h = append(h, "RIFF"...)
	le32(uint32(36 + len(data)))
	h = append(h, "WAVEfmt "...)
	le32(16)
	le16(1) // PCM
	le16(uint16(channels))
	le32(uint32(rate))
	le32(uint32(rate * channels * 2))
	le16(uint16(channels * 2))
	le16(16)
	h = append(h, "data"...)
	le32(uint32(len(data)))

	return os.WriteFile(path, append(h, data...), 0o644)
}

func tool(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, out)
	}
	return nil
}

func concat(dst string, srcs ...string) error {
	var all []byte
	for _, s := range srcs {
		b, err := os.ReadFile(s)
		if err != nil {
			return err
		}
		all = append(all, b...)
	}
	return os.WriteFile(dst, all, 0o644)
}
