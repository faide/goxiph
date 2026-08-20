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
		}
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
