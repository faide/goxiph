package opus

import (
	"bufio"
	"encoding/hex"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// plcCase is one run of packets with losses in it, and what the reference played.
type plcCase struct {
	name string
	// packets is one entry per position, nil where the packet was thrown away, and samples holds
	// how many the lost one should produce.
	packets [][]byte
	samples []int
	// want holds the reference's interleaved output for the positions it printed, by position.
	want map[int][]int
}

// readPLCVectors parses testdata/opus_plc_vectors.txt.
func readPLCVectors(t *testing.T) []plcCase {
	t.Helper()

	f, err := os.Open("testdata/opus_plc_vectors.txt")
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
				name: strings.Join(fields[1:], " "),
				want: map[int][]int{},
			})
			cur = &cases[len(cases)-1]
		case "packethex":
			b, err := hex.DecodeString(fields[2])
			if err != nil {
				t.Fatalf("packet: %v", err)
			}
			cur.packets = append(cur.packets, b)
			cur.samples = append(cur.samples, 0)
		case "lost":
			cur.packets = append(cur.packets, nil)
			cur.samples = append(cur.samples, num(fields[2]))
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

// TestConcealmentMatchesReference decodes runs of packets with losses in them and checks every
// concealed frame, and every frame that follows one, against the reference.
//
// A concealed frame is not pinned down by the format, but it is by the reference. Checking the frame
// after a loss is the half that matters most: concealment writes the history the next packet decodes
// from, so a decoder that conceals differently drifts out of step afterwards even where the
// concealed frame itself sounded right.
//
// The comparison is to within a few parts in ten thousand rather than exact, because the reference
// output is quantised to sixteen bits and ours is not.
func TestConcealmentMatchesReference(t *testing.T) {
	cases := readPLCVectors(t)
	if len(cases) == 0 {
		t.Skip("no concealment vectors")
	}

	const tolerance = 5e-4
	checked, worst := 0, 0.0

	for _, c := range cases {
		dec, err := NewDecoder(2)
		if err != nil {
			t.Fatal(err)
		}

		for k, packet := range c.packets {
			var got [][]float32
			if packet == nil {
				got, err = dec.Conceal(c.samples[k])
			} else {
				got, err = dec.Decode(packet)
			}
			if err != nil {
				t.Fatalf("%s packet %d: %v", c.name, k, err)
			}

			want, ok := c.want[k]
			if !ok {
				continue
			}
			if len(got)*len(got[0]) != len(want) {
				t.Fatalf("%s packet %d: %d samples, reference has %d",
					c.name, k, len(got)*len(got[0]), len(want))
			}
			for j := range got[0] {
				for ch := range got {
					d := math.Abs(float64(got[ch][j]) - float64(want[j*len(got)+ch])/32768)
					worst = math.Max(worst, d)
					if d > tolerance {
						t.Fatalf("%s packet %d sample %d channel %d: %v, reference has %v",
							c.name, k, j, ch, got[ch][j], float64(want[j*len(got)+ch])/32768)
					}
				}
			}
			checked++
		}
	}
	t.Logf("%d concealed and recovered frames match the reference across %d runs, worst %.2e",
		checked, len(cases), worst)
}
