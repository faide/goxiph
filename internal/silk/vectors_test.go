package silk

import (
	"bufio"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/faide/goxiph/internal/rangecoder"
)

// vectorCase is one payload from testdata/silk_vectors.txt and what the reference decoded from it.
type vectorCase struct {
	config  Config
	payload []byte

	vad  [2][]int
	lbrr [2]int
	// lbrrFrames is indexed by channel.
	lbrrFrames [2][]int
}

// readVectors parses the reference dump.
//
// The file records every stage, and only the ones a test needs are read; the rest are carried along
// so that adding a stage means reading more fields rather than regenerating anything.
func readVectors(t *testing.T) []vectorCase {
	t.Helper()

	f, err := os.Open("testdata/silk_vectors.txt")
	if err != nil {
		t.Fatalf("reading vectors: %v", err)
	}
	defer f.Close()

	var cases []vectorCase
	var cur *vectorCase

	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<20), 1<<20)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		atoi := func(i int) int {
			v, err := strconv.Atoi(fields[i])
			if err != nil {
				t.Fatalf("parsing %q: %v", fields[i], err)
			}
			return v
		}

		switch fields[0] {
		case "case":
			cases = append(cases, vectorCase{
				config: Config{SampleRateKHz: atoi(1), FrameMS: atoi(2), Channels: atoi(3)},
			})
			cur = &cases[len(cases)-1]
		case "payloadhex":
			b, err := hex.DecodeString(fields[1])
			if err != nil {
				t.Fatalf("payload: %v", err)
			}
			cur.payload = b
		case "vad":
			ch := atoi(1)
			for i := 2; i < len(fields) && fields[i] != "lbrr"; i++ {
				cur.vad[ch] = append(cur.vad[ch], atoi(i))
			}
			cur.lbrr[ch] = atoi(len(fields) - 1)
		case "lbrrflags":
			ch := atoi(1)
			for i := 2; i < len(fields); i++ {
				cur.lbrrFrames[ch] = append(cur.lbrrFrames[ch], atoi(i))
			}
		}
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no cases in the vector file")
	}
	return cases
}

// TestHeaderMatchesReference checks the packet flags against what libopus read from the same bytes.
//
// The flags are one bit each and cost almost nothing, which is exactly why an error here is hard to
// see later: the wrong number of them shifts every symbol that follows, and the frame decodes to
// noise rather than to anything recognisably wrong.
func TestHeaderMatchesReference(t *testing.T) {
	cases := readVectors(t)
	checked := 0

	for i, c := range cases {
		dec := rangecoder.NewDecoder(c.payload)
		h, err := DecodeHeader(dec, c.config)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}

		for ch := range c.config.Channels {
			for f, want := range c.vad[ch] {
				if got := h.VAD[ch][f]; got != (want != 0) {
					t.Fatalf("case %d channel %d frame %d: activity %v, reference has %d",
						i, ch, f, got, want)
				}
			}
			if got := h.LBRR[ch]; got != (c.lbrr[ch] != 0) {
				t.Fatalf("case %d channel %d: redundancy %v, reference has %d",
					i, ch, got, c.lbrr[ch])
			}
			for f, want := range c.lbrrFrames[ch] {
				if got := h.LBRRFrame[ch][f]; got != (want != 0) {
					t.Fatalf("case %d channel %d frame %d: redundant %v, reference has %d",
						i, ch, f, got, want)
				}
			}
		}
		checked++
	}
	t.Logf("%d packet headers match the reference", checked)
}

// TestVectorsCoverBothBandwidths guards the corpus rather than the code: a vector file that had lost
// one of the two rates would still pass every test above it.
func TestVectorsCoverBothBandwidths(t *testing.T) {
	rates := map[int]int{}
	active, inactive := 0, 0
	for _, c := range readVectors(t) {
		rates[c.config.SampleRateKHz]++
		for _, v := range c.vad[0] {
			if v != 0 {
				active++
			} else {
				inactive++
			}
		}
	}
	for _, want := range []int{NarrowBand, WideBand} {
		if rates[want] == 0 {
			t.Errorf("no vectors at %d kHz", want)
		}
	}
	t.Logf("vectors by rate: %v; %d active frames, %d inactive", rates, active, inactive)
}
