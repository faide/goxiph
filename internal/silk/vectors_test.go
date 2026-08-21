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

	// frames holds what the reference decoded for each played frame, in order.
	frames []refFrame
}

// refFrame is one frame's worth of the reference's decode.
type refFrame struct {
	index, channel int
	signalType     int
	quantOffset    int
	interpolation  int
	seed           int
	gainIndices    []int
	nlsfIndices    []int

	voiced        bool
	lagIndex      int
	contourIndex  int
	periodicity   int
	ltpScaleIndex int
	ltpIndices    []int

	// pulses is the excitation the reference decoded, one entry per sample.
	pulses []int

	// gainsQ16 and the two coefficient sets are what the indices expand into.
	gainsQ16 []int
	lpc1     []int
	lpc0     []int

	pitchLags   []int
	ltpCoefQ14  []int
	ltpScaleQ14 int

	// samples is the synthesised output the reference produced, and resampled the same at 48 kHz.
	samples   []int
	resampled []int
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
		case "frame":
			cur.frames = append(cur.frames, refFrame{
				index: atoi(1), channel: atoi(2),
				signalType: atoi(4), quantOffset: atoi(5),
				interpolation: atoi(7), seed: atoi(9),
			})
		case "gainidx":
			f := &cur.frames[len(cur.frames)-1]
			for i := 3; i < len(fields); i++ {
				f.gainIndices = append(f.gainIndices, atoi(i))
			}
		case "nlsfidx":
			f := &cur.frames[len(cur.frames)-1]
			for i := 3; i < len(fields); i++ {
				f.nlsfIndices = append(f.nlsfIndices, atoi(i))
			}
		case "pulses":
			f := &cur.frames[len(cur.frames)-1]
			for i := 4; i < len(fields); i++ {
				f.pulses = append(f.pulses, atoi(i))
			}
		case "gainsq16":
			f := &cur.frames[len(cur.frames)-1]
			for i := 3; i < len(fields); i++ {
				f.gainsQ16 = append(f.gainsQ16, atoi(i))
			}
		case "lpc":
			f := &cur.frames[len(cur.frames)-1]
			for i := 4; i < len(fields); i++ {
				f.lpc1 = append(f.lpc1, atoi(i))
			}
		case "lpc0":
			f := &cur.frames[len(cur.frames)-1]
			for i := 3; i < len(fields); i++ {
				f.lpc0 = append(f.lpc0, atoi(i))
			}
		case "pitchl":
			f := &cur.frames[len(cur.frames)-1]
			for i := 3; i < len(fields); i++ {
				f.pitchLags = append(f.pitchLags, atoi(i))
			}
		case "ltpcoef":
			f := &cur.frames[len(cur.frames)-1]
			for i := 3; i < len(fields); i++ {
				f.ltpCoefQ14 = append(f.ltpCoefQ14, atoi(i))
			}
		case "ltpscaleq14":
			cur.frames[len(cur.frames)-1].ltpScaleQ14 = atoi(3)
		case "out":
			f := &cur.frames[len(cur.frames)-1]
			for i := 4; i < len(fields); i++ {
				f.samples = append(f.samples, atoi(i))
			}
		case "resampled":
			f := &cur.frames[len(cur.frames)-1]
			for i := 4; i < len(fields); i++ {
				f.resampled = append(f.resampled, atoi(i))
			}
		case "pitch":
			f := &cur.frames[len(cur.frames)-1]
			f.voiced = true
			f.lagIndex, f.contourIndex = atoi(4), atoi(6)
			f.periodicity, f.ltpScaleIndex = atoi(8), atoi(10)
			for i := 12; i < len(fields); i++ {
				f.ltpIndices = append(f.ltpIndices, atoi(i))
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

// TestFrameSymbolsMatchReference checks each frame's codebook selections and excitation against
// libopus.
//
// Everything here is read from one bitstream in a fixed order, so a symbol taken from the wrong
// distribution or skipped shifts everything after it. Comparing the decoded fields rather than the
// bit position is what says which one went wrong; comparing the bit position alone would only say
// that something had.
//
// The excitation is decoded here rather than in a test of its own because it has to be: it sits
// between one frame's indices and the next frame's, and skipping it desynchronises the rest of the
// packet. That is how the multi-frame case first failed.
func TestFrameSymbolsMatchReference(t *testing.T) {
	cases := readVectors(t)
	frames, voiced, conditional := 0, 0, 0
	subframeCounts := map[int]int{}
	nonZeroExcitation := 0

	for i, c := range cases {
		dec := rangecoder.NewDecoder(c.payload)
		h, err := DecodeHeader(dec, c.config)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if h.LBRR[0] || h.LBRR[1] {
			// A packet carrying redundancy has frames before these that must be read and discarded.
			// Nothing in the corpus does, so rather than guess at it, skip and say so.
			continue
		}

		var st frameState
		for _, want := range c.frames {
			mode := CodeIndependently
			if want.index > 0 {
				mode = CodeConditionally
				conditional++
			}
			subframeCounts[c.config.Subframes()]++
			got := DecodeIndices(dec, c.config, &st, mode, h.VAD[want.channel][want.index], false)

			check := func(name string, a, b int) {
				t.Helper()
				if a != b {
					t.Fatalf("case %d frame %d channel %d: %s is %d, reference has %d",
						i, want.index, want.channel, name, a, b)
				}
			}
			check("signal type", got.SignalType, want.signalType)
			check("quantiser offset", got.QuantOffsetType, want.quantOffset)
			check("interpolation", got.NLSFInterpolation, want.interpolation)
			check("seed", got.Seed, want.seed)
			for k, w := range want.gainIndices[:c.config.Subframes()] {
				check("gain index", got.GainIndices[k], w)
			}
			for k, w := range want.nlsfIndices {
				check("line spectral index", got.NLSFIndices[k], w)
			}
			if want.voiced {
				voiced++
				check("pitch lag", got.LagIndex, want.lagIndex)
				check("contour", got.ContourIndex, want.contourIndex)
				check("periodicity", got.Periodicity, want.periodicity)
				check("long-term scale", got.LTPScaleIndex, want.ltpScaleIndex)
				for k, w := range want.ltpIndices[:c.config.Subframes()] {
					check("long-term gain", got.LTPIndices[k], w)
				}
			}

			// The excitation follows the indices and must be consumed before the next frame's, so
			// this is not an optional extra even for a test of the indices alone.
			ex := DecodePulses(dec, got.SignalType, got.QuantOffsetType, c.config.FrameLength())
			if len(ex) != len(want.pulses) {
				t.Fatalf("case %d frame %d: %d excitation samples, reference has %d",
					i, want.index, len(ex), len(want.pulses))
			}
			for k := range ex {
				if ex[k] != want.pulses[k] {
					t.Fatalf("case %d frame %d sample %d: excitation %d, reference has %d",
						i, want.index, k, ex[k], want.pulses[k])
				}
			}
			for _, v := range ex {
				if v != 0 {
					nonZeroExcitation++
				}
			}
			frames++
		}
	}

	// Each of these is a distinct path through the decoder, and a corpus that lost one would leave
	// it passing while testing nothing.
	if conditional == 0 {
		t.Error("no frame used conditional coding; delta gains and delta pitch went untested")
	}
	if len(subframeCounts) < 2 {
		t.Errorf("only one subframe layout was seen: %v", subframeCounts)
	}
	if nonZeroExcitation == 0 {
		t.Error("every excitation decoded to silence")
	}
	t.Logf("%d conditional frames, subframe layouts %v, %d non-zero excitation samples",
		conditional, subframeCounts, nonZeroExcitation)

	if frames == 0 {
		t.Fatal("no frames were checked")
	}
	if voiced == 0 {
		t.Error("no voiced frame was checked; the pitch fields went untested")
	}
	t.Logf("%d frames match the reference, %d of them voiced", frames, voiced)
}
