// Command vectors surveys the official Opus test vectors.
//
// The generated corpus is made of tones and noise, which an encoder answers with a narrow slice of
// the format. The official vectors are built to exercise the rest, so what they contain says which
// of the decoder's paths a corpus of our own cannot reach.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/faide/goxiph/opus"
)

func main() {
	dir := flag.String("dir", "", "directory holding testvectorNN.bit")
	flag.Parse()

	files, err := filepath.Glob(filepath.Join(*dir, "testvector*.bit"))
	if err != nil || len(files) == 0 {
		fmt.Fprintln(os.Stderr, "vectors: no test vectors in", *dir)
		os.Exit(1)
	}
	sort.Strings(files)

	total := map[string]int{}
	frameSizes := map[int]int{}
	packets := 0

	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "vectors:", err)
			os.Exit(1)
		}
		per := map[string]int{}
		n := 0
		for i := 0; i+8 <= len(raw); {
			length := int(binary.BigEndian.Uint32(raw[i:]))
			i += 8 // the four bytes after the length hold the expected final range
			if length < 0 || i+length > len(raw) {
				break
			}
			p, err := opus.ParsePacket(raw[i : i+length])
			i += length
			n++
			packets++
			if err != nil {
				per["unparsed"]++
				continue
			}
			ch := "M"
			if p.Stereo {
				ch = "S"
			}
			key := fmt.Sprintf("%s/%s/%s", p.Mode, p.Bandwidth, ch)
			per[key]++
			total[key]++
			frameSizes[p.Samples()/len(p.Frames)]++
		}
		fmt.Printf("%-18s %4d packets  %s\n", filepath.Base(path), n, brief(per))
	}

	fmt.Printf("\n%d packets across %d vectors\n", packets, len(files))
	keys := make([]string, 0, len(total))
	for k := range total {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-18s %5d\n", k, total[k])
	}
	sizes := make([]int, 0, len(frameSizes))
	for k := range frameSizes {
		sizes = append(sizes, k)
	}
	sort.Ints(sizes)
	fmt.Print("  frame sizes:")
	for _, s := range sizes {
		fmt.Printf(" %d(%d)", s, frameSizes[s])
	}
	fmt.Println()
}

func brief(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for _, k := range keys {
		out += fmt.Sprintf("%s:%d ", k, m[k])
	}
	return out
}
