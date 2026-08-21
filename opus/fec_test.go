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

// fecCase is one stream carrying in-band redundancy, with the positions that were thrown away.
type fecCase struct {
	name    string
	packets [][]byte
	// lost says the packet at that position never arrived, and samples how long it was. recovered
	// says the packet after it carried a copy to stand in.
	lost      []bool
	recovered []bool
	samples   []int
	// want holds the reference's output for the positions it printed, by position.
	want map[int][]int
}

// readFECVectors parses testdata/opus_fec_vectors.txt.
func readFECVectors(t *testing.T) []fecCase {
	t.Helper()

	f, err := os.Open("testdata/opus_fec_vectors.txt")
	if err != nil {
		t.Skipf("no redundancy vectors: %v", err)
	}
	defer f.Close()

	num := func(s string) int {
		v, err := strconv.Atoi(s)
		if err != nil {
			t.Fatalf("parsing %q: %v", s, err)
		}
		return v
	}

	var cases []fecCase
	var cur *fecCase
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<22), 1<<22)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		switch fields[0] {
		case "case":
			cases = append(cases, fecCase{name: strings.Join(fields[1:], " "), want: map[int][]int{}})
			cur = &cases[len(cases)-1]
		case "packethex":
			b, err := hex.DecodeString(fields[2])
			if err != nil {
				t.Fatalf("packet: %v", err)
			}
			cur.packets = append(cur.packets, b)
			cur.lost = append(cur.lost, false)
			cur.recovered = append(cur.recovered, false)
			cur.samples = append(cur.samples, 0)
		case "lost", "recover":
			i := num(fields[1])
			cur.lost[i] = true
			cur.recovered[i] = fields[0] == "recover"
			cur.samples[i] = num(fields[2])
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

// TestRedundancyMatchesReference checks that a lost packet recovered from the next packet's
// redundancy plays what the reference plays, and that the packet after it still decodes.
//
// The redundancy covers only the bands the linear predictor codes; the rest is extrapolated. So this
// exercises both halves at once, and the frame after the recovery is what says the state came out
// right — reading a packet for its redundancy and reading it for its own frames leave the decoder in
// different places, and the next packet is decoded from one of them.
func TestRedundancyMatchesReference(t *testing.T) {
	cases := readFECVectors(t)
	if len(cases) == 0 {
		t.Skip("no redundancy vectors")
	}

	const tolerance = 1e-4
	checked, recovered, worst := 0, 0, 0.0

	for _, c := range cases {
		dec, err := NewDecoder(1)
		if err != nil {
			t.Fatal(err)
		}

		for k, packet := range c.packets {
			var got [][]float32
			switch {
			case c.recovered[k]:
				got, err = dec.DecodeFEC(c.packets[k+1], c.samples[k])
				recovered++
			case c.lost[k]:
				got, err = dec.Conceal(c.samples[k])
			default:
				got, err = dec.Decode(packet)
			}
			if err != nil {
				t.Fatalf("%s packet %d: %v", c.name, k, err)
			}

			want, ok := c.want[k]
			if !ok {
				continue
			}
			if len(got[0]) != len(want) {
				t.Fatalf("%s packet %d: %d samples, reference has %d", c.name, k, len(got[0]), len(want))
			}
			for j := range want {
				d := math.Abs(float64(got[0][j]) - float64(want[j])/32768)
				worst = math.Max(worst, d)
				if d > tolerance {
					t.Fatalf("%s packet %d sample %d: %v, reference has %v",
						c.name, k, j, got[0][j], float64(want[j])/32768)
				}
			}
			checked++
		}
	}
	t.Logf("%d recovered packets and %d checked frames match the reference across %d streams, worst %.2e",
		recovered, checked, len(cases), worst)
}
