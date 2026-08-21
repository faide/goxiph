// Command silkframes extracts the SILK payloads from the generated corpus.
//
// The reference oracle in internal/silk/testdata takes one payload at a time, so the frames have to
// come out of their Ogg pages and packet framing first. Its output feeds that oracle; neither is run
// at test time, and the vectors they produce are what the tests read.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/faide/goxiph/ogg"
	"github.com/faide/goxiph/opus"
)

// rateFor maps an Opus bandwidth onto SILK's internal rate. Anything wider is not SILK's to decode.
var rateFor = map[opus.Bandwidth]int{
	opus.BandwidthNarrow: 8,
	opus.BandwidthMedium: 12,
	opus.BandwidthWide:   16,
}

func main() {
	out := flag.String("out", "", "directory to write payloads into")
	in := flag.String("in", "testdata/generated", "corpus directory")
	limit := flag.Int("limit", 240, "most payloads to write")
	flag.Parse()

	if *out == "" {
		fmt.Fprintln(os.Stderr, "silkframes: -out is required")
		os.Exit(1)
	}
	n, err := run(*in, *out, *limit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "silkframes:", err)
		os.Exit(1)
	}
	fmt.Printf("silkframes: wrote %d payloads to %s\n", n, *out)
}

func run(in, out string, limit int) (int, error) {
	files, err := filepath.Glob(filepath.Join(in, "*.opus"))
	if err != nil {
		return 0, err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return 0, err
	}

	written := 0
	for _, path := range files {
		n, err := extract(path, out, limit-written, written)
		written += n
		if err != nil {
			return written, err
		}
		if written >= limit {
			break
		}
	}
	return written, nil
}

func extract(path, out string, budget, index int) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()

	d := ogg.NewDemuxer(f)
	headers, written := 0, 0

	for written < budget {
		p, err := d.ReadPacket()
		if err != nil {
			return written, nil
		}
		// The identification and comment packets carry no audio.
		if headers < 2 {
			headers++
			continue
		}

		pk, err := opus.ParsePacket(p.Data)
		if err != nil || pk.Mode != opus.ModeSILK {
			continue
		}
		rate, ok := rateFor[pk.Bandwidth]
		if !ok {
			continue
		}
		channels := 1
		if pk.Stereo {
			channels = 2
		}
		ms := pk.Samples() / len(pk.Frames) / 48

		for _, frame := range pk.Frames {
			if len(frame) <= 1 || written >= budget {
				continue
			}
			// The name carries the configuration, because the payload does not.
			name := fmt.Sprintf("%s_%03d_%d_%d_%d.bin",
				filepath.Base(path), index+written, rate, ms, channels)
			if err := os.WriteFile(filepath.Join(out, name), frame, 0o644); err != nil {
				return written, err
			}
			written++
		}
	}
	return written, nil
}
