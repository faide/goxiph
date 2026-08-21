package celt

import (
	"math/rand/v2"
	"testing"

	"github.com/faide/goxiph/internal/rangecoder"
)

// allocate runs the whole allocation path over arbitrary bytes.
//
// The allocation reads from the range decoder, so it cannot be driven by supplying values directly;
// feeding it bytes is how the real decoder reaches it, and a decoder past the end of its data
// supplies zeros rather than failing.
func allocate(t *testing.T, data []byte, start, end int, frame FrameSize, channels, total int) Allocation {
	t.Helper()
	d := rangecoder.NewDecoder(data)
	caps := Caps(frame, channels)

	var offsets [NumBands]int
	totalBits := len(data) * 8 << BitRes
	boost := DecodeBoosts(d, &offsets, start, end, frame, channels, &caps, totalBits)
	trim := DecodeTrim(d, totalBits, boost)

	return ComputeAllocation(d, start, end, &offsets, &caps, trim, total, frame, channels, false)
}

// TestTrimICDFMatchesPDF checks the trim distribution against the probabilities RFC 6716 table 58
// prints, rather than against the reference's copy of the same numbers.
func TestTrimICDFMatchesPDF(t *testing.T) {
	pdf := []int{2, 2, 5, 10, 22, 46, 22, 10, 5, 2, 2}

	total := 0
	for _, p := range pdf {
		total += p
	}
	if total != 128 {
		t.Fatalf("the printed distribution sums to %d, want 128", total)
	}

	cum := 0
	for i, p := range pdf {
		cum += p
		// The inverse cumulative form counts down from the total.
		if got := int(trimICDF[i]); got != 128-cum {
			t.Errorf("trim symbol %d: icdf %d, want %d", i, got, 128-cum)
		}
	}
	if trimICDF[len(trimICDF)-1] != 0 {
		t.Error("the distribution does not end at zero")
	}
}

// TestCapsAreOrdered checks the per-band maxima for the shape they must have.
func TestCapsAreOrdered(t *testing.T) {
	for _, frame := range []FrameSize{Frame2p5ms, Frame5ms, Frame10ms, Frame20ms} {
		for _, channels := range []int{1, 2} {
			caps := Caps(frame, channels)
			for b := range NumBands {
				if caps[b] <= 0 {
					t.Errorf("frame %d channels %d band %d: cap %d", frame, channels, b, caps[b])
				}
			}
			// A wider band can hold more, and stereo holds more than mono.
			mono := Caps(frame, 1)
			if channels == 2 {
				for b := range NumBands {
					if caps[b] <= mono[b] {
						t.Errorf("band %d: stereo cap %d does not exceed mono %d", b, caps[b], mono[b])
					}
				}
			}
		}
	}
	// A longer frame holds more in the same band.
	short := Caps(Frame2p5ms, 1)
	long := Caps(Frame20ms, 1)
	for b := range NumBands {
		if long[b] <= short[b] {
			t.Errorf("band %d: 20 ms cap %d does not exceed 2.5 ms %d", b, long[b], short[b])
		}
	}
}

// TestAllocationInvariants is the strongest check available before the decoder is whole.
//
// The allocation cannot be compared against a reference here, but it can be required to be usable:
// nothing negative, nothing past a band's cap, fine energy within its limit, and the coded bands
// forming a prefix. A violation of any of these makes the shape decoder read the wrong number of
// symbols, so they are not cosmetic.
func TestAllocationInvariants(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 17))

	for _, frame := range []FrameSize{Frame2p5ms, Frame5ms, Frame10ms, Frame20ms} {
		for _, channels := range []int{1, 2} {
			for _, total := range []int{0, 8, 64, 512, 4096, 40000} {
				data := make([]byte, 64)
				for i := range data {
					data[i] = byte(rng.UintN(256))
				}

				a := allocate(t, data, 0, NumBands, frame, channels, total)
				caps := Caps(frame, channels)

				if a.CodedBands < 0 || a.CodedBands > NumBands {
					t.Fatalf("frame %d ch %d total %d: CodedBands %d", frame, channels, total, a.CodedBands)
				}
				for b := range NumBands {
					if a.Pulses[b] < 0 {
						t.Fatalf("band %d has %d pulses", b, a.Pulses[b])
					}
					if a.Pulses[b] > caps[b] {
						t.Fatalf("band %d has %d pulses, over its cap of %d", b, a.Pulses[b], caps[b])
					}
					if a.FineBits[b] < 0 || a.FineBits[b] > MaxFineBits {
						t.Fatalf("band %d has %d fine bits", b, a.FineBits[b])
					}
					if a.FinePriority[b] != 0 && a.FinePriority[b] != 1 {
						t.Fatalf("band %d has priority %d", b, a.FinePriority[b])
					}
					// A band past the coded prefix carries no shape.
					if b >= a.CodedBands && a.Pulses[b] != 0 {
						t.Fatalf("skipped band %d still holds %d pulses", b, a.Pulses[b])
					}
				}
				if a.Intensity < 0 || a.Intensity > NumBands {
					t.Fatalf("Intensity %d", a.Intensity)
				}
				if channels == 1 && a.DualStereo {
					t.Fatal("a mono frame reported dual stereo")
				}
			}
		}
	}
}

// TestAllocationGrowsWithCapacity pins the property the interpolation exists to provide.
func TestAllocationGrowsWithCapacity(t *testing.T) {
	data := make([]byte, 256) // zeros decode to no boosts and the default trim

	sum := func(a Allocation) int {
		total := 0
		for b := range NumBands {
			total += a.Pulses[b] + a.FineBits[b]<<BitRes
		}
		return total
	}

	prev := -1
	for _, total := range []int{64, 256, 1024, 4096, 16384} {
		a := allocate(t, data, 0, NumBands, Frame20ms, 1, total)
		got := sum(a)
		if got < prev {
			t.Errorf("capacity %d allocated %d, less than the %d allocated before it", total, got, prev)
		}
		prev = got
	}
}

// TestTrimBiasesTowardsHighBands checks the trim's stated purpose against the bias it computes.
//
// An end-to-end check is the wrong instrument here: whether a band survives depends on skip flags
// read from the bitstream, so an arbitrary input can skip every high band and leave nothing to
// measure. The bias itself is deterministic, so it is what gets tested.
func TestTrimBiasesTowardsHighBands(t *testing.T) {
	const frame, channels = Frame20ms, 1

	neutral := TrimOffsets(0, NumBands, 5, frame, channels)
	low := TrimOffsets(0, NumBands, 1, frame, channels)
	high := TrimOffsets(0, NumBands, 9, frame, channels)

	// The tilt pivots at the top band, so the lowest band moves most and the top one not at all.
	for b := range NumBands - 1 {
		if low[b] >= neutral[b] || neutral[b] >= high[b] {
			t.Fatalf("band %d offsets are %d, %d, %d for trims 1, 5 and 9; want them to rise",
				b, low[b], neutral[b], high[b])
		}
		// The tilt is per bin, not per band: it scales with the band's width as well as with its
		// distance from the top, so a wide band high up can carry more total bias than a narrow one
		// below it. Dividing by the width is what makes the ordering visible.
		if b > 0 {
			perBin := high[b] / (BandEdges[b+1] - BandEdges[b])
			prevPerBin := high[b-1] / (BandEdges[b] - BandEdges[b-1])
			if perBin >= prevPerBin {
				t.Errorf("band %d is tilted %d per bin, not less than band %d at %d",
					b, perBin, b-1, prevPerBin)
			}
		}
	}
	if high[NumBands-1] != neutral[NumBands-1] {
		t.Errorf("the top band moved from %d to %d; it is the pivot and should not",
			neutral[NumBands-1], high[NumBands-1])
	}
}

// TestTrimNeutralIsSmallAtTheDefault checks that the default trim of five leaves the allocation
// close to the table, which is what "no trim" has to mean.
func TestTrimNeutralIsSmallAtTheDefault(t *testing.T) {
	// At the shortest frame the frame-size term vanishes and neutral trim is exactly zero.
	offsets := TrimOffsets(0, NumBands, 5, Frame2p5ms, 1)
	for b := range NumBands {
		n := BandEdges[b+1] - BandEdges[b]
		want := 0
		if n == 1 {
			want = -1 << BitRes // the one-bin adjustment still applies
		}
		if offsets[b] != want {
			t.Errorf("band %d neutral offset %d, want %d", b, offsets[b], want)
		}
	}
}

// TestAllocationIsDeterministic pins the property RFC 6716 section 4.3.3 demands: the same input
// must produce the same allocation every time, because the encoder made the same computation.
func TestAllocationIsDeterministic(t *testing.T) {
	data := []byte{0x5A, 0xC3, 0x11, 0x9E, 0x77, 0x02, 0xFF, 0x40}

	first := allocate(t, data, 0, NumBands, Frame10ms, 2, 2000)
	for range 10 {
		again := allocate(t, data, 0, NumBands, Frame10ms, 2, 2000)
		if again != first {
			t.Fatal("the same input produced a different allocation")
		}
	}
}

// TestHybridStartBand covers the case where CELT begins part-way up the spectrum, which is how the
// hybrid mode hands the low bands to SILK.
func TestHybridStartBand(t *testing.T) {
	const start = 17
	a := allocate(t, make([]byte, 64), start, NumBands, Frame20ms, 1, 4000)

	for b := range start {
		if a.Pulses[b] != 0 {
			t.Errorf("band %d is below the start band but holds %d pulses", b, a.Pulses[b])
		}
	}
	if a.CodedBands <= start {
		t.Errorf("CodedBands = %d, not above the start band %d", a.CodedBands, start)
	}
}

// TestZeroCapacityAllocatesNothing covers the degenerate frame.
func TestZeroCapacityAllocatesNothing(t *testing.T) {
	a := allocate(t, nil, 0, NumBands, Frame20ms, 1, 0)
	for b := range NumBands {
		if a.Pulses[b] != 0 {
			t.Errorf("band %d holds %d pulses from an empty frame", b, a.Pulses[b])
		}
	}
}

// TestBoostsAreBounded covers the dynamic allocation loop, which reads a variable number of symbols
// and so must terminate on any input.
func TestBoostsAreBounded(t *testing.T) {
	rng := rand.New(rand.NewPCG(23, 29))

	for range 200 {
		data := make([]byte, rng.IntN(64)+1)
		for i := range data {
			data[i] = byte(rng.UintN(256))
		}
		frame := FrameSize(rng.IntN(4))
		channels := rng.IntN(2) + 1

		caps := Caps(frame, channels)
		var offsets [NumBands]int
		d := rangecoder.NewDecoder(data)
		boost := DecodeBoosts(d, &offsets, 0, NumBands, frame, channels, &caps, len(data)*8<<BitRes)

		if boost < 0 {
			t.Fatalf("total boost %d", boost)
		}
		for b := range NumBands {
			if offsets[b] < 0 {
				t.Fatalf("band %d boosted by %d", b, offsets[b])
			}
			if offsets[b] > caps[b] {
				t.Fatalf("band %d boosted to %d, over its cap of %d", b, offsets[b], caps[b])
			}
		}
	}
}

func FuzzAllocation(f *testing.F) {
	f.Add([]byte{0, 0, 0, 0}, uint8(3), uint8(1), uint16(4000))
	f.Add([]byte{0xFF, 0xFF}, uint8(0), uint8(2), uint16(64))

	f.Fuzz(func(t *testing.T, data []byte, frameRaw, channelsRaw uint8, totalRaw uint16) {
		frame := FrameSize(frameRaw % 4)
		channels := int(channelsRaw%2) + 1
		total := int(totalRaw)

		caps := Caps(frame, channels)
		var offsets [NumBands]int
		d := rangecoder.NewDecoder(data)
		totalBits := len(data) * 8 << BitRes
		boost := DecodeBoosts(d, &offsets, 0, NumBands, frame, channels, &caps, totalBits)
		trim := DecodeTrim(d, totalBits, boost)
		a := ComputeAllocation(d, 0, NumBands, &offsets, &caps, trim, total, frame, channels, false)

		// Whatever the input, the result has to be usable by the stages that follow.
		for b := range NumBands {
			if a.Pulses[b] < 0 || a.Pulses[b] > caps[b] {
				t.Fatalf("band %d: %d pulses against a cap of %d", b, a.Pulses[b], caps[b])
			}
			if a.FineBits[b] < 0 || a.FineBits[b] > MaxFineBits {
				t.Fatalf("band %d: %d fine bits", b, a.FineBits[b])
			}
		}
		if a.CodedBands < 0 || a.CodedBands > NumBands {
			t.Fatalf("CodedBands %d", a.CodedBands)
		}
	})
}

func BenchmarkComputeAllocation(b *testing.B) {
	caps := Caps(Frame20ms, 2)
	var offsets [NumBands]int
	data := make([]byte, 256)

	b.ReportAllocs()
	for b.Loop() {
		d := rangecoder.NewDecoder(data)
		ComputeAllocation(d, 0, NumBands, &offsets, &caps, 5, 8000, Frame20ms, 2, false)
	}
}

// encodeBoosts mirrors the reference's dynamic allocation loop on the encoding side.
//
// Its stopping condition is the point of it: the budget it tests against shrinks as boosts are
// spent, so a decoder that kept testing the original budget would keep reading where the encoder had
// stopped writing.
// It returns the boosts it wrote, which are whole quanta and so may overshoot what was
// asked for; the decoder has to agree with those rather than with the request.
func encodeBoosts(e *rangecoder.Encoder, want []int, start, end int,
	frame FrameSize, channels int, caps *[NumBands]int, totalBits int,
) []int {
	written := make([]int, NumBands)
	dynallocLogp := uint32(6)
	for b := start; b < end; b++ {
		width := channels * (BandEdges[b+1] - BandEdges[b]) << frame
		quanta := min(8*width, max(48, width))

		boost := 0
		loopLogp := dynallocLogp
		for e.TellFrac()+int(loopLogp)<<BitRes < totalBits && boost < caps[b] {
			flag := 0
			if boost < want[b] {
				flag = 1
			}
			e.EncodeBitLogp(flag, loopLogp)
			if flag == 0 {
				break
			}
			boost += quanta
			totalBits -= quanta
			loopLogp = 1
		}
		written[b] = boost
		if boost > 0 && dynallocLogp > 2 {
			dynallocLogp--
		}
	}
	return written
}

// TestBoostsRoundTripAgainstTheEncoderLoop is what checks the stopping condition rather than the
// result.
//
// A boost loop that reads too far still terminates and still respects the caps, so the bounds tests
// pass either way; only running the encoder's own condition against it shows a disagreement. The
// budgets here are chosen tight enough that the condition binds partway through the frame.
func TestBoostsRoundTripAgainstTheEncoderLoop(t *testing.T) {
	bound := 0
	for _, frame := range []FrameSize{Frame2p5ms, Frame5ms, Frame10ms, Frame20ms} {
		for _, channels := range []int{1, 2} {
			caps := Caps(frame, channels)

			for _, scale := range []int{1, 2, 4, 8} {
				want := make([]int, NumBands)
				for b := range NumBands {
					width := channels * (BandEdges[b+1] - BandEdges[b]) << frame
					quanta := min(8*width, max(48, width))
					want[b] = quanta * (b % 3) * scale
					want[b] = min(want[b], caps[b])
				}

				for _, budget := range []int{200, 400, 800, 1600, 3200, 12800} {
					enc := rangecoder.NewEncoder(1 << 14)
					written := encodeBoosts(enc, want, 0, NumBands, frame, channels, &caps, budget)
					data := enc.Done()

					var got [NumBands]int
					dec := rangecoder.NewDecoder(data)
					DecodeBoosts(dec, &got, 0, NumBands, frame, channels, &caps, budget)

					for b := range NumBands {
						if got[b] != written[b] {
							t.Fatalf("frame %d channels %d budget %d band %d: decoded a boost of %d, encoder wrote %d",
								frame, channels, budget, b, got[b], written[b])
						}
					}
					// The two must also stop at the same place, or everything after desynchronises.
					if dec.TellFrac() != enc.TellFrac() {
						t.Fatalf("frame %d channels %d budget %d: decoder stopped at %d, encoder at %d",
							frame, channels, budget, dec.TellFrac(), enc.TellFrac())
					}
					// The budget bound the loop where a band stopped short of both the request and
					// its cap. Without one such case the condition under test never fired.
					for b := range NumBands {
						if written[b] < want[b] && written[b] < caps[b] {
							bound++
						}
					}
				}
			}
		}
	}
	if bound == 0 {
		t.Fatal("no budget was tight enough to bind; the stopping condition was never tested")
	}
	t.Logf("the budget bound the loop in %d cases", bound)
}
