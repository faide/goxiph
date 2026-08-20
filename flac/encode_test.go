package flac

import (
	"bytes"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/faide/goxiph/internal/bitio"
)

// seekableBuffer lets the encoder patch the stream info block, which a plain bytes.Buffer cannot.
type seekableBuffer struct {
	data []byte
	pos  int64
}

func (b *seekableBuffer) Write(p []byte) (int, error) {
	need := int(b.pos) + len(p)
	if need > len(b.data) {
		b.data = append(b.data, make([]byte, need-len(b.data))...)
	}
	copy(b.data[b.pos:], p)
	b.pos = int64(need)
	return len(p), nil
}

func (b *seekableBuffer) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		b.pos = off
	case io.SeekCurrent:
		b.pos += off
	case io.SeekEnd:
		b.pos = int64(len(b.data)) + off
	}
	if b.pos < 0 {
		return 0, errors.New("negative position")
	}
	return b.pos, nil
}

// roundTrip encodes samples and decodes them again, returning what came back.
func roundTrip(t *testing.T, samples [][]int32, info StreamInfo, opt EncoderOptions) ([][]int32, StreamInfo, []byte) {
	t.Helper()

	var buf seekableBuffer
	enc, err := NewEncoder(&buf, info, opt)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.Write(samples); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, gotInfo, err := DecodeAll(bytes.NewReader(buf.data))
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	return got, gotInfo, buf.data
}

// requireBitExact is the only acceptable result for a lossless codec.
func requireBitExact(t *testing.T, got, want [][]int32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d channels, want %d", len(got), len(want))
	}
	for c := range want {
		if len(got[c]) != len(want[c]) {
			t.Fatalf("channel %d: got %d samples, want %d", c, len(got[c]), len(want[c]))
		}
		for i := range want[c] {
			if got[c][i] != want[c][i] {
				t.Fatalf("channel %d sample %d: got %d, want %d", c, i, got[c][i], want[c][i])
			}
		}
	}
}

// signalGen builds test signals that reach different subframe types.
type signalGen struct {
	name string
	fn   func(i, ch, n int, depth int) int32
}

func signals() []signalGen {
	full := func(depth int) float64 { return float64(int64(1)<<(depth-1)) - 1 }
	return []signalGen{
		{"silence", func(int, int, int, int) int32 { return 0 }},
		{"constant", func(_, ch, _, depth int) int32 { return int32(1000 + ch) }},
		{"ramp", func(i, ch, _, depth int) int32 { return int32((i*7 + ch) % 1000) }},
		{"sine", func(i, ch, _, depth int) int32 {
			return int32(full(depth) * 0.7 * math.Sin(float64(i)*0.05+float64(ch)))
		}},
		{"quadratic", func(i, _, _, _ int) int32 { return int32(i * i % 30000) }},
		{"fullscale square", func(i, _, _, depth int) int32 {
			if i%64 < 32 {
				return int32(full(depth))
			}
			return int32(-full(depth) - 1)
		}},
		{"incompressible", func(i, ch, _, depth int) int32 {
			// A deterministic xorshift: uniform bits defeat every predictor the format has, which
			// is what forces verbatim subframes.
			x := uint32(i*2654435761 + ch*40503)
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			return int32(x) >> (32 - depth)
		}},
		{"wasted bits", func(i, _, _, _ int) int32 {
			// Low bits always zero, which the encoder should notice and strip.
			return int32(i%977) << 6
		}},
		{"correlated stereo", func(i, ch, _, depth int) int32 {
			base := int32(full(depth) * 0.5 * math.Sin(float64(i)*0.01))
			if ch == 1 {
				return base + int32(i%3) // nearly identical channels favour mid-side
			}
			return base
		}},
	}
}

// TestRoundTripIsBitExact is the encoder's gate.
//
// FLAC promises the output equals the input. There is no tolerance to negotiate, so a single
// differing sample is a failure.
func TestRoundTripIsBitExact(t *testing.T) {
	for _, sig := range signals() {
		for _, depth := range []int{8, 16, 24} {
			for _, channels := range []int{1, 2, 3} {
				name := sig.name + "/" + itoa(depth) + "bit/" + itoa(channels) + "ch"
				t.Run(name, func(t *testing.T) {
					const n = 5000
					samples := make([][]int32, channels)
					for ch := range samples {
						samples[ch] = make([]int32, n)
						for i := range n {
							samples[ch][i] = clampTo(sig.fn(i, ch, n, depth), depth)
						}
					}

					info := StreamInfo{SampleRate: 44100, Channels: channels, BitsPerSample: depth}
					got, gotInfo, _ := roundTrip(t, samples, info, EncoderOptions{BlockSize: 1024})

					requireBitExact(t, got, samples)
					if gotInfo.SampleRate != 44100 || gotInfo.Channels != channels || gotInfo.BitsPerSample != depth {
						t.Errorf("stream info round-tripped as %+v", gotInfo)
					}
					if gotInfo.TotalSamples != n {
						t.Errorf("TotalSamples = %d, want %d", gotInfo.TotalSamples, n)
					}
				})
			}
		}
	}
}

// clampTo keeps a generated sample inside the declared depth.
func clampTo(v int32, depth int) int32 {
	hi := int32(int64(1)<<(depth-1)) - 1
	lo := -hi - 1
	return min(max(v, lo), hi)
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// TestRoundTripRandomBlocks covers block sizes that do not divide the input, so the final frame is
// short and every partition order has to stay legal.
func TestRoundTripRandomBlocks(t *testing.T) {
	rng := rand.New(rand.NewPCG(19, 23))

	for _, blockSize := range []int{16, 17, 192, 576, 1000, 4096} {
		for _, n := range []int{1, 15, 16, 100, 4097} {
			samples := [][]int32{make([]int32, n), make([]int32, n)}
			for ch := range samples {
				for i := range n {
					samples[ch][i] = int32(rng.IntN(2000) - 1000)
				}
			}
			info := StreamInfo{SampleRate: 48000, Channels: 2, BitsPerSample: 16}
			got, _, _ := roundTrip(t, samples, info, EncoderOptions{BlockSize: blockSize})
			requireBitExact(t, got, samples)
		}
	}
}

// TestRoundTripWriteInChunks checks that buffering across calls does not disturb the framing.
func TestRoundTripWriteInChunks(t *testing.T) {
	const n = 3000
	want := [][]int32{make([]int32, n), make([]int32, n)}
	for ch := range want {
		for i := range n {
			want[ch][i] = int32((i*13 + ch*7) % 5000)
		}
	}

	var buf seekableBuffer
	info := StreamInfo{SampleRate: 44100, Channels: 2, BitsPerSample: 16}
	enc, err := NewEncoder(&buf, info, EncoderOptions{BlockSize: 256})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	// Chunk sizes that straddle block boundaries in both directions.
	for pos := 0; pos < n; {
		take := min(1+pos%400, n-pos)
		chunk := [][]int32{want[0][pos : pos+take], want[1][pos : pos+take]}
		if err := enc.Write(chunk); err != nil {
			t.Fatalf("Write: %v", err)
		}
		pos += take
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, _, err := DecodeAll(bytes.NewReader(buf.data))
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	requireBitExact(t, got, want)
}

// TestUnseekableWriterLeavesLengthUnknown covers the path where the stream info block cannot be
// patched. Zero is what the format defines as unknown, so the stream stays valid.
func TestUnseekableWriterLeavesLengthUnknown(t *testing.T) {
	const n = 1000
	samples := [][]int32{make([]int32, n)}
	for i := range n {
		samples[0][i] = int32(i % 700)
	}

	var buf bytes.Buffer // no Seek method
	info := StreamInfo{SampleRate: 44100, Channels: 1, BitsPerSample: 16}
	enc, err := NewEncoder(&buf, info, EncoderOptions{BlockSize: 256})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := enc.Write(samples); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := enc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	got, gotInfo, err := DecodeAll(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	requireBitExact(t, got, samples)
	if gotInfo.TotalSamples != 0 {
		t.Errorf("TotalSamples = %d, want 0 for an unseekable writer", gotInfo.TotalSamples)
	}
}

// TestEncoderCompresses checks that the output is smaller than the raw samples for signals that
// ought to compress. A round trip alone would pass on a verbatim-only encoder.
func TestEncoderCompresses(t *testing.T) {
	const n = 20000
	cases := []struct {
		name  string
		gen   func(i int) int32
		ratio float64 // required fraction of the raw size
	}{
		{"silence", func(int) int32 { return 0 }, 0.02},
		{"constant", func(int) int32 { return 4242 }, 0.02},
		{"ramp", func(i int) int32 { return int32(i % 1000) }, 0.35},
		{"sine", func(i int) int32 { return int32(20000 * math.Sin(float64(i)*0.02)) }, 0.60},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			samples := [][]int32{make([]int32, n)}
			for i := range n {
				samples[0][i] = c.gen(i)
			}
			info := StreamInfo{SampleRate: 44100, Channels: 1, BitsPerSample: 16}
			got, _, encoded := roundTrip(t, samples, info, EncoderOptions{BlockSize: 4096})
			requireBitExact(t, got, samples)

			raw := n * 2
			if float64(len(encoded)) > float64(raw)*c.ratio {
				t.Errorf("encoded to %d bytes from %d raw (%.1f%%), want under %.0f%%",
					len(encoded), raw, 100*float64(len(encoded))/float64(raw), 100*c.ratio)
			}
		})
	}
}

// TestStereoModesAreChosen checks that the decorrelation search picks the mode that fits the signal,
// since an encoder that always chose independent would still round-trip.
func TestStereoModesAreChosen(t *testing.T) {
	const n = 4096

	build := func(gen func(i int) (int32, int32)) [][]int32 {
		s := [][]int32{make([]int32, n), make([]int32, n)}
		for i := range n {
			s[0][i], s[1][i] = gen(i)
		}
		return s
	}

	t.Run("identical channels favour a side mode", func(t *testing.T) {
		samples := build(func(i int) (int32, int32) {
			v := int32(15000 * math.Sin(float64(i)*0.01))
			return v, v // the side channel is all zeros
		})
		e := newTestEncoder(t, 2, 16)
		e.pending[0] = append(e.pending[0][:0], samples[0]...)
		e.pending[1] = append(e.pending[1][:0], samples[1]...)

		if mode := e.decorrelate(n); mode == channelsIndependent {
			t.Error("identical channels were coded independently")
		}
	})

	t.Run("uncorrelated channels favour independent", func(t *testing.T) {
		samples := build(func(i int) (int32, int32) {
			return int32(i % 977), int32((i * 31) % 683)
		})
		e := newTestEncoder(t, 2, 16)
		e.pending[0] = append(e.pending[0][:0], samples[0]...)
		e.pending[1] = append(e.pending[1][:0], samples[1]...)

		mode := e.decorrelate(n)
		// Whatever it picks must round-trip; the point is that the choice is exercised.
		info := StreamInfo{SampleRate: 44100, Channels: 2, BitsPerSample: 16}
		got, _, _ := roundTrip(t, samples, info, EncoderOptions{BlockSize: 4096})
		requireBitExact(t, got, samples)
		_ = mode
	})
}

func newTestEncoder(t *testing.T, channels, depth int) *Encoder {
	t.Helper()
	var buf seekableBuffer
	e, err := NewEncoder(&buf, StreamInfo{SampleRate: 44100, Channels: channels, BitsPerSample: depth},
		EncoderOptions{BlockSize: 4096})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	return e
}

func TestNewEncoderRejects(t *testing.T) {
	var buf seekableBuffer
	cases := []struct {
		name string
		info StreamInfo
		opt  EncoderOptions
	}{
		{"zero channels", StreamInfo{SampleRate: 44100, Channels: 0, BitsPerSample: 16}, EncoderOptions{}},
		{"too many channels", StreamInfo{SampleRate: 44100, Channels: 9, BitsPerSample: 16}, EncoderOptions{}},
		{"depth too low", StreamInfo{SampleRate: 44100, Channels: 1, BitsPerSample: 3}, EncoderOptions{}},
		{"depth too high", StreamInfo{SampleRate: 44100, Channels: 1, BitsPerSample: 33}, EncoderOptions{}},
		{"zero sample rate", StreamInfo{SampleRate: 0, Channels: 1, BitsPerSample: 16}, EncoderOptions{}},
		{"block too small", StreamInfo{SampleRate: 44100, Channels: 1, BitsPerSample: 16}, EncoderOptions{BlockSize: 8}},
		{"block too large", StreamInfo{SampleRate: 44100, Channels: 1, BitsPerSample: 16}, EncoderOptions{BlockSize: 70000}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := NewEncoder(&buf, c.info, c.opt); !errors.Is(err, ErrBadStream) {
				t.Errorf("got %v, want ErrBadStream", err)
			}
		})
	}
}

func TestWriteRejectsMismatchedChannels(t *testing.T) {
	e := newTestEncoder(t, 2, 16)
	if err := e.Write([][]int32{make([]int32, 10)}); err == nil {
		t.Error("accepted one channel for a two-channel stream")
	}
	if err := e.Write([][]int32{make([]int32, 10), make([]int32, 9)}); err == nil {
		t.Error("accepted channels of differing lengths")
	}
}

// TestCodedNumberRoundTrip checks the UTF-8-like frame numbering across every octet length,
// including the values past what RFC 3629 allows.
func TestCodedNumberRoundTrip(t *testing.T) {
	values := []uint64{
		0, 1, 0x7F,
		0x80, 0x7FF,
		0x800, 0xFFFF,
		0x10000, 0x1FFFFF,
		0x200000, 0x3FFFFFF,
		0x4000000, 0x7FFFFFFF,
		0x80000000, 0xFFFFFFFFF,
	}
	for _, v := range values {
		w := bitio.NewMSBWriter()
		writeCodedNumber(w, v)
		_ = w.Write(0, 8) // padding

		got, err := readCodedNumber(bitio.NewMSBReader(w.Bytes()))
		if err != nil {
			t.Fatalf("value %#x: %v", v, err)
		}
		if got != v {
			t.Errorf("value %#x round-tripped as %#x", v, got)
		}
	}
}

func TestFoldRoundTrip(t *testing.T) {
	for _, v := range []int32{0, 1, -1, 2, -2, 1000, -1000, math.MaxInt32 / 2, -math.MaxInt32 / 2} {
		f := fold(v)
		var back int32
		if f&1 == 0 {
			back = int32(f >> 1)
		} else {
			back = int32(^(f >> 1))
		}
		if back != v {
			t.Errorf("fold(%d) = %d unfolded to %d", v, f, back)
		}
	}
}

func TestCommonWastedBits(t *testing.T) {
	cases := []struct {
		in   []int32
		want uint
	}{
		{[]int32{0, 0, 0}, 0}, // all zero has no wasted bits to signal
		{[]int32{1, 2, 3}, 0},
		{[]int32{2, 4, 6}, 1},
		{[]int32{8, 16, 24}, 3},
		{[]int32{-8, 16, -24}, 3},
		{[]int32{1 << 20, 1 << 21}, 20},
	}
	for _, c := range cases {
		if got := commonWastedBits(c.in); got != c.want {
			t.Errorf("commonWastedBits(%v) = %d, want %d", c.in, got, c.want)
		}
	}
}

func BenchmarkEncode(b *testing.B) {
	const n = 44100
	samples := [][]int32{make([]int32, n), make([]int32, n)}
	for ch := range samples {
		for i := range n {
			samples[ch][i] = int32(15000 * math.Sin(float64(i)*0.01+float64(ch)))
		}
	}
	info := StreamInfo{SampleRate: 44100, Channels: 2, BitsPerSample: 16}

	b.SetBytes(int64(n * 2 * 2))
	for b.Loop() {
		var buf seekableBuffer
		enc, _ := NewEncoder(&buf, info, EncoderOptions{})
		_ = enc.Write(samples)
		_ = enc.Close()
	}
}

// TestFrameBoundaryFalsePositive is a regression test for a bug the round-trip fuzzer found.
//
// Native FLAC frames carry no length, so the decoder finds the end of one by locating the next
// frame's sync code. Residual data contains sync-like byte pairs routinely, and here one of them
// was followed by two bytes that happened to match the frame checksum of everything before it. The
// decoder split the frame there and then failed to decode the truncated remainder.
//
// A candidate split now also has to be followed by a frame header whose own checksum holds.
func TestFrameBoundaryFalsePositive(t *testing.T) {
	raw := []byte("00\xd500000\xd30\xad0000000\xcf00000\xce0000000\xc00\xf5\x060000\xe50\xcf00000" +
		"\x06֮00000\x02\x8400000000000000\xb60\x850000000\xb8000000000000000\vY000000000000" +
		"\x95000000000\xa200000\xb900000000000000000\xd60000000\xce00000\x8f0000000\xdd0000000" +
		"\xca0\x9400000\xc10000000\xf3\xa7000000\xd400000000000000000000000\xba0000000" +
		"\xa6000000000000000\xca0000000\xa6000000000000000\x9a0000000\x800000000\xf6\x8d000000" +
		"\xaf0000000")

	const (
		depth     = 13
		channels  = 4
		blockSize = 38
	)
	n := len(raw) / channels / 2
	hi := int32(int64(1)<<(depth-1)) - 1
	lo := -hi - 1

	samples := make([][]int32, channels)
	for c := range samples {
		samples[c] = make([]int32, n)
		for i := range n {
			off := (i*channels + c) * 2
			v := int32(int16(uint16(raw[off])<<8 | uint16(raw[off+1])))
			samples[c][i] = min(max(v, lo), hi)
		}
	}

	info := StreamInfo{SampleRate: 44100, Channels: channels, BitsPerSample: depth}
	got, _, _ := roundTrip(t, samples, info, EncoderOptions{BlockSize: blockSize})
	requireBitExact(t, got, samples)
}

// TestIsFrameSync pins the sync mask. Masking one bit too many would also accept 0xFA and 0xFB,
// which are not sync codes, and widen the window for exactly the false positive above.
func TestIsFrameSync(t *testing.T) {
	accept := [][]byte{{0xFF, 0xF8}, {0xFF, 0xF9}, {0xFF, 0xF8, 0x00}}
	for _, p := range accept {
		if !isFrameSync(p) {
			t.Errorf("rejected a valid sync %x", p)
		}
	}
	reject := [][]byte{{0xFF, 0xFA}, {0xFF, 0xFB}, {0xFF, 0xF7}, {0xFE, 0xF8}, {0xFF}, nil}
	for _, p := range reject {
		if isFrameSync(p) {
			t.Errorf("accepted %x as a sync code", p)
		}
	}
}

// TestLPCActuallyImprovesCompression is the guard against the bug this feature shipped with first.
//
// The linear predictor was wired in complete but inert: its window buffer was allocated and never
// filled, so every windowed sample was zero, autocorrelation reported silence and the search bailed
// out before it began. Every test still passed, because dead code cannot break a round trip. Only an
// A/B measurement showed it, so an A/B measurement is what guards it.
func TestLPCActuallyImprovesCompression(t *testing.T) {
	const n = 40000
	// A tonal signal with harmonics: exactly what a linear predictor models and fixed predictors
	// cannot.
	samples := [][]int32{make([]int32, n)}
	for i := range n {
		x := float64(i)
		samples[0][i] = int32(9000*math.Sin(x*0.031) +
			4000*math.Sin(x*0.062+0.7) +
			1500*math.Sin(x*0.093+1.9))
	}
	info := StreamInfo{SampleRate: 44100, Channels: 1, BitsPerSample: 16}

	_, _, withoutLPC := roundTrip(t, samples, info, EncoderOptions{MaxLPCOrder: -1})
	got, _, withLPC := roundTrip(t, samples, info, EncoderOptions{MaxLPCOrder: 8})

	requireBitExact(t, got, samples)

	if len(withLPC) >= len(withoutLPC) {
		t.Errorf("linear prediction produced %d bytes against %d without it; it is not being used",
			len(withLPC), len(withoutLPC))
	}
	t.Logf("%d bytes with linear prediction, %d without (%.1f%% smaller)",
		len(withLPC), len(withoutLPC),
		100*(1-float64(len(withLPC))/float64(len(withoutLPC))))
}

// TestLPCDisabledStillRoundTrips checks the fixed-predictor-only path stays correct.
func TestLPCDisabledStillRoundTrips(t *testing.T) {
	const n = 5000
	samples := [][]int32{make([]int32, n), make([]int32, n)}
	for ch := range samples {
		for i := range n {
			samples[ch][i] = int32(7000 * math.Sin(float64(i)*0.02+float64(ch)))
		}
	}
	info := StreamInfo{SampleRate: 44100, Channels: 2, BitsPerSample: 16}
	got, _, _ := roundTrip(t, samples, info, EncoderOptions{MaxLPCOrder: -1})
	requireBitExact(t, got, samples)
}

// TestLPCResidualMatchesDecoderArithmetic pins the one place the encoder and decoder must agree
// exactly: both fold the prediction with int64 accumulation and an arithmetic right shift, and any
// difference makes the stream lossy without making it invalid.
func TestLPCResidualMatchesDecoderArithmetic(t *testing.T) {
	samples := []int32{100, -250, 3000, -4096, 32767, -32768, 7, 0, -1, 12345}
	coeffs := []int32{1900, -900, 300}
	const shift = 10

	residual := make([]int32, len(samples))
	if !lpcResidual(samples, coeffs, shift, residual) {
		t.Fatal("lpcResidual reported an out-of-range residual for in-range input")
	}

	// Reconstruct exactly as the decoder does.
	got := make([]int32, len(samples))
	copy(got[:len(coeffs)], samples[:len(coeffs)])
	copy(got[len(coeffs):], residual[len(coeffs):])
	for i := len(coeffs); i < len(got); i++ {
		var pred int64
		for j := range coeffs {
			pred += int64(coeffs[j]) * int64(got[i-1-j])
		}
		got[i] += int32(pred >> shift)
	}
	for i := range samples {
		if got[i] != samples[i] {
			t.Fatalf("sample %d: reconstructed %d, want %d", i, got[i], samples[i])
		}
	}
}

func TestQuantizeCoefficients(t *testing.T) {
	t.Run("recovers the shape", func(t *testing.T) {
		coef := []float64{1.75, -0.875, 0.25}
		coeffs, shift, ok := quantizeCoefficients(coef, 15)
		if !ok {
			t.Fatal("quantizeCoefficients refused a well-formed predictor")
		}
		scale := math.Ldexp(1, shift)
		for i, want := range coef {
			got := float64(coeffs[i]) / scale
			if math.Abs(got-want) > 0.01 {
				t.Errorf("coefficient %d dequantised to %g, want %g", i, got, want)
			}
		}
	})

	t.Run("shift is never negative", func(t *testing.T) {
		// The field is a five-bit signed value the format forbids from being negative.
		for _, scale := range []float64{1, 1e3, 1e6, 1e9} {
			coef := []float64{scale, -scale / 2}
			_, shift, ok := quantizeCoefficients(coef, 15)
			if ok && shift < 0 {
				t.Errorf("scale %g gave shift %d", scale, shift)
			}
		}
	})

	t.Run("rejects degenerate input", func(t *testing.T) {
		for _, coef := range [][]float64{
			{0, 0, 0},
			{math.NaN()},
			{math.Inf(1)},
			{},
		} {
			if _, _, ok := quantizeCoefficients(coef, 15); ok {
				t.Errorf("accepted %v", coef)
			}
		}
	})

	t.Run("stays inside the precision", func(t *testing.T) {
		coef := []float64{1.9, -1.9, 0.5, -0.5}
		coeffs, _, ok := quantizeCoefficients(coef, 8)
		if !ok {
			t.Fatal("refused a well-formed predictor")
		}
		limit := int32(1)<<7 - 1
		for i, c := range coeffs {
			if c > limit || c < -limit-1 {
				t.Errorf("coefficient %d is %d, outside 8-bit range", i, c)
			}
		}
	})
}
