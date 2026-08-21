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

// plcCase is one sequence of payloads with losses in it, and what the reference played.
type plcCase struct {
	config Config
	// packets is one entry per position: a payload, or nil where the packet was lost.
	packets [][]byte
	// want holds the reference's samples for the positions it printed, by position.
	want map[int][]int
}

// readPLCVectors parses testdata/silk_plc_vectors.txt.
func readPLCVectors(t *testing.T) []plcCase {
	t.Helper()

	f, err := os.Open("testdata/silk_plc_vectors.txt")
	if err != nil {
		t.Skipf("no concealment vectors: %v", err)
	}
	defer f.Close()

	num := func(s string) int {
		v, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("parsing %q: %v", s, err)
		}
		return v
	}

	var cases []plcCase
	var cur *plcCase
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<22), 1<<22)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		switch fields[0] {
		case "case":
			cases = append(cases, plcCase{
				config: Config{
					SampleRateKHz: num(fields[2]),
					FrameMS:       num(fields[4]),
					Channels:      num(fields[6]),
				},
				want: map[int][]int{},
			})
			cur = &cases[len(cases)-1]
		case "payloadhex":
			b, err := hex.DecodeString(fields[1])
			if err != nil {
				t.Fatalf("payload: %v", err)
			}
			cur.packets = append(cur.packets, b)
		case "lost":
			cur.packets = append(cur.packets, nil)
		case "out":
			v := make([]int, 0, num(fields[2]))
			for _, x := range fields[3:] {
				v = append(v, num(x))
			}
			cur.want[num(fields[1])] = v
		}
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return cases
}

// TestConcealmentMatchesReference decodes sequences with losses in them and checks every concealed
// frame, and every frame that follows one, against the reference.
//
// A concealed frame is not pinned down by the format, but it is by the reference: every decoder that
// runs the same extrapolation produces the same samples, and a decoder that does not will drift out
// of step on the frames after it, because concealment writes the history the next frame predicts
// from. Checking the frame after a loss is what catches that.
func TestConcealmentMatchesReference(t *testing.T) {
	cases := readPLCVectors(t)
	if len(cases) == 0 {
		t.Skip("no concealment vectors")
	}

	checked := 0
	for i, c := range cases {
		d, err := NewDecoder(c.config)
		if err != nil {
			t.Fatal(err)
		}

		for k, payload := range c.packets {
			var got [][]int16
			if payload == nil {
				got, err = d.Conceal(c.config.FrameMS)
			} else {
				got, err = d.Decode(rangecoder.NewDecoder(payload), c.config.FrameMS)
			}
			if err != nil {
				t.Fatalf("case %d packet %d: %v", i, k, err)
			}

			want, ok := c.want[k]
			if !ok {
				continue
			}
			// The reference interleaves; ours is one slice per channel.
			if len(got)*len(got[0]) != len(want) {
				t.Fatalf("case %d packet %d: %d samples, reference has %d",
					i, k, len(got)*len(got[0]), len(want))
			}
			for j := range got[0] {
				for ch := range got {
					if int(got[ch][j]) != want[j*len(got)+ch] {
						t.Fatalf("case %d (%d kHz, %d ms) packet %d sample %d channel %d: %d, reference has %d",
							i, c.config.SampleRateKHz, c.config.FrameMS, k, j, ch,
							got[ch][j], want[j*len(got)+ch])
					}
				}
			}
			checked++
		}
	}
	t.Logf("%d concealed and recovered frames match the reference across %d sequences",
		checked, len(cases))
}
