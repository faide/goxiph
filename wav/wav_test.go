package wav

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
)

// seekableBuffer lets the encoder patch the RIFF and data lengths, which a plain bytes.Buffer
// cannot.
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

func encodeInts(t *testing.T, format Format, samples [][]int32) []byte {
	t.Helper()
	var buf seekableBuffer
	e, err := NewEncoder(&buf, format)
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := e.WriteInt32(samples); err != nil {
		t.Fatalf("WriteInt32: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return buf.data
}

func requireEqual(t *testing.T, got, want [][]int32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d channels, want %d", len(got), len(want))
	}
	for c := range want {
		if len(got[c]) != len(want[c]) {
			t.Fatalf("channel %d: got %d frames, want %d", c, len(got[c]), len(want[c]))
		}
		for i := range want[c] {
			if got[c][i] != want[c][i] {
				t.Fatalf("channel %d frame %d: got %d, want %d", c, i, got[c][i], want[c][i])
			}
		}
	}
}

// TestIntegerRoundTrip covers every integer depth at the extremes of its range, where a sign or
// bias mistake shows.
func TestIntegerRoundTrip(t *testing.T) {
	for _, depth := range []int{8, 16, 24, 32} {
		for _, channels := range []int{1, 2, 6} {
			t.Run(depthName(depth)+"/"+depthName(channels)+"ch", func(t *testing.T) {
				hi := int32(int64(1)<<(depth-1)) - 1
				lo := -hi - 1
				values := []int32{0, 1, -1, hi, lo, hi / 2, lo / 2, 42, -42}

				samples := make([][]int32, channels)
				for ch := range samples {
					samples[ch] = make([]int32, len(values))
					copy(samples[ch], values)
				}

				format := Format{SampleRate: 44100, Channels: channels, BitsPerSample: depth}
				data := encodeInts(t, format, samples)

				got, gotFormat, err := DecodeAllInt32(bytes.NewReader(data))
				if err != nil {
					t.Fatalf("DecodeAllInt32: %v", err)
				}
				if gotFormat.SampleRate != 44100 || gotFormat.Channels != channels ||
					gotFormat.BitsPerSample != depth || gotFormat.Float {
					t.Errorf("format round-tripped as %+v", gotFormat)
				}
				requireEqual(t, got, samples)
			})
		}
	}
}

func depthName(v int) string {
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

// TestEightBitIsUnsigned pins the one depth that breaks the pattern.
//
// Eight-bit WAVE samples are unsigned with a bias of 128; every wider depth is signed. Reading them
// as signed puts the waveform half a scale out of place, which sounds like loud distortion rather
// than like a bug.
func TestEightBitIsUnsigned(t *testing.T) {
	samples := [][]int32{{-128, -1, 0, 1, 127}}
	data := encodeInts(t, Format{SampleRate: 8000, Channels: 1, BitsPerSample: 8}, samples)

	// Find the data chunk and check the stored bytes directly.
	idx := bytes.Index(data, []byte(idData))
	if idx < 0 {
		t.Fatal("no data chunk")
	}
	stored := data[idx+8 : idx+8+5]
	want := []byte{0x00, 0x7F, 0x80, 0x81, 0xFF}
	if !bytes.Equal(stored, want) {
		t.Errorf("stored %x, want %x", stored, want)
	}

	got, _, err := DecodeAllInt32(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeAllInt32: %v", err)
	}
	requireEqual(t, got, samples)
}

// TestExtensibleHeader checks that formats the classic header cannot express get the extensible one.
func TestExtensibleHeader(t *testing.T) {
	t.Run("many channels", func(t *testing.T) {
		samples := make([][]int32, 6)
		for ch := range samples {
			samples[ch] = []int32{int32(ch), int32(-ch)}
		}
		format := Format{SampleRate: 48000, Channels: 6, BitsPerSample: 24, ChannelMask: 0x3F}
		data := encodeInts(t, format, samples)

		got, gotFormat, err := DecodeAllInt32(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("DecodeAllInt32: %v", err)
		}
		if gotFormat.ChannelMask != 0x3F {
			t.Errorf("ChannelMask = %#x, want 0x3f", gotFormat.ChannelMask)
		}
		requireEqual(t, got, samples)
	})

	t.Run("stereo stays classic", func(t *testing.T) {
		samples := [][]int32{{1}, {2}}
		data := encodeInts(t, Format{SampleRate: 44100, Channels: 2, BitsPerSample: 16}, samples)
		// A 16-byte format chunk is the classic layout; 40 would be extensible.
		idx := bytes.Index(data, []byte(idFmt))
		if idx < 0 {
			t.Fatal("no format chunk")
		}
		if size := data[idx+4]; size != 16 {
			t.Errorf("format chunk is %d bytes, want 16 for plain stereo", size)
		}
	})
}

func TestFloatRoundTrip(t *testing.T) {
	for _, depth := range []int{32, 64} {
		t.Run(depthName(depth), func(t *testing.T) {
			values := []float32{0, 1, -1, 0.5, -0.5, 1e-6, -1e-6}
			samples := [][]float32{values, values}

			var buf seekableBuffer
			format := Format{SampleRate: 96000, Channels: 2, BitsPerSample: depth, Float: true}
			e, err := NewEncoder(&buf, format)
			if err != nil {
				t.Fatalf("NewEncoder: %v", err)
			}
			if err := e.WriteFloat32(samples); err != nil {
				t.Fatalf("WriteFloat32: %v", err)
			}
			if err := e.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}

			d, err := NewDecoder(bytes.NewReader(buf.data))
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			if !d.Format().Float {
				t.Error("format did not round-trip as float")
			}

			got := [][]float32{make([]float32, len(values)), make([]float32, len(values))}
			n, err := d.ReadFloat32(got)
			if err != nil {
				t.Fatalf("ReadFloat32: %v", err)
			}
			if n != len(values) {
				t.Fatalf("read %d frames, want %d", n, len(values))
			}
			for ch := range got {
				for i := range values {
					if got[ch][i] != values[i] {
						t.Errorf("channel %d frame %d: got %v, want %v", ch, i, got[ch][i], values[i])
					}
				}
			}
		})
	}
}

// TestFloatFileRejectsIntegerRead covers the boundary where a silent conversion would otherwise pick
// a scale the caller did not choose.
func TestFloatFileRejectsIntegerRead(t *testing.T) {
	var buf seekableBuffer
	e, err := NewEncoder(&buf, Format{SampleRate: 44100, Channels: 1, BitsPerSample: 32, Float: true})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	_ = e.WriteFloat32([][]float32{{0.5}})
	_ = e.Close()

	d, err := NewDecoder(bytes.NewReader(buf.data))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if _, err := d.ReadInt32([][]int32{make([]int32, 1)}); err == nil {
		t.Error("ReadInt32 accepted a float file")
	}
}

// TestOddLengthDataChunkIsPadded covers the RIFF alignment rule. A chunk with an odd size is
// followed by a pad byte that belongs to no chunk, and missing it desynchronises everything after.
func TestOddLengthDataChunkIsPadded(t *testing.T) {
	// One 8-bit mono frame gives a one-byte data chunk.
	samples := [][]int32{{42}}
	data := encodeInts(t, Format{SampleRate: 8000, Channels: 1, BitsPerSample: 8}, samples)

	if len(data)%2 != 0 {
		t.Errorf("file is %d bytes, want an even length after padding", len(data))
	}
	got, _, err := DecodeAllInt32(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("DecodeAllInt32: %v", err)
	}
	requireEqual(t, got, samples)
}

// TestSkipsUnknownChunks checks that metadata between the header and the samples is stepped over,
// including an odd-length one that exercises the pad rule.
func TestSkipsUnknownChunks(t *testing.T) {
	samples := [][]int32{{100, -100, 0}}
	clean := encodeInts(t, Format{SampleRate: 44100, Channels: 1, BitsPerSample: 16}, samples)

	fmtEnd := bytes.Index(clean, []byte(idData))
	if fmtEnd < 0 {
		t.Fatal("no data chunk")
	}

	// LIST of odd length, then a zero-length chunk, spliced ahead of the data chunk.
	var extra []byte
	extra = append(extra, "LIST"...)
	extra = append(extra, 5, 0, 0, 0)
	extra = append(extra, "INFOx"...)
	extra = append(extra, 0) // pad byte for the odd size
	extra = append(extra, "JUNK"...)
	extra = append(extra, 0, 0, 0, 0)

	spliced := make([]byte, 0, len(clean)+len(extra))
	spliced = append(spliced, clean[:fmtEnd]...)
	spliced = append(spliced, extra...)
	spliced = append(spliced, clean[fmtEnd:]...)

	got, _, err := DecodeAllInt32(bytes.NewReader(spliced))
	if err != nil {
		t.Fatalf("DecodeAllInt32: %v", err)
	}
	requireEqual(t, got, samples)
}

// TestStreamingWriterOmitsLengths covers the unseekable path, where the lengths cannot be patched.
func TestStreamingWriterOmitsLengths(t *testing.T) {
	samples := [][]int32{{1, 2, 3, 4}}

	var buf bytes.Buffer // no Seek method
	e, err := NewEncoder(&buf, Format{SampleRate: 44100, Channels: 1, BitsPerSample: 16})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := e.WriteInt32(samples); err != nil {
		t.Fatalf("WriteInt32: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The reader falls back to reading until the end of the file.
	got, _, err := DecodeAllInt32(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("DecodeAllInt32: %v", err)
	}
	requireEqual(t, got, samples)
}

func TestFramesReported(t *testing.T) {
	samples := [][]int32{make([]int32, 100), make([]int32, 100)}
	data := encodeInts(t, Format{SampleRate: 44100, Channels: 2, BitsPerSample: 16}, samples)

	d, err := NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if got := d.Frames(); got != 100 {
		t.Errorf("Frames = %d, want 100", got)
	}
}

func TestDecoderRejects(t *testing.T) {
	good := encodeInts(t, Format{SampleRate: 44100, Channels: 1, BitsPerSample: 16}, [][]int32{{1, 2}})

	cases := []struct {
		name string
		data []byte
	}{
		{"empty", nil},
		{"short", good[:8]},
		{"not RIFF", append([]byte("RIFX"), good[4:]...)},
		{"not WAVE", append(append([]byte{}, good[:8]...), append([]byte("AVI "), good[12:]...)...)},
		{"truncated before data", good[:20]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, err := DecodeAllInt32(bytes.NewReader(c.data)); err == nil {
				t.Error("accepted a malformed file")
			}
		})
	}
}

func TestFormatValidate(t *testing.T) {
	bad := []Format{
		{SampleRate: 0, Channels: 1, BitsPerSample: 16},
		{SampleRate: 44100, Channels: 0, BitsPerSample: 16},
		{SampleRate: 44100, Channels: 1, BitsPerSample: 12},
		{SampleRate: 44100, Channels: 1, BitsPerSample: 20},
		{SampleRate: 44100, Channels: 1, BitsPerSample: 16, Float: true},
	}
	for _, f := range bad {
		if err := f.Validate(); err == nil {
			t.Errorf("accepted %+v", f)
		}
	}
	ok := []Format{
		{SampleRate: 44100, Channels: 1, BitsPerSample: 8},
		{SampleRate: 44100, Channels: 8, BitsPerSample: 32},
		{SampleRate: 44100, Channels: 2, BitsPerSample: 32, Float: true},
		{SampleRate: 44100, Channels: 2, BitsPerSample: 64, Float: true},
	}
	for _, f := range ok {
		if err := f.Validate(); err != nil {
			t.Errorf("rejected %+v: %v", f, err)
		}
	}
}

func TestWriteRejectsMismatchedChannels(t *testing.T) {
	var buf seekableBuffer
	e, err := NewEncoder(&buf, Format{SampleRate: 44100, Channels: 2, BitsPerSample: 16})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := e.WriteInt32([][]int32{make([]int32, 4)}); err == nil {
		t.Error("accepted one channel for a stereo format")
	}
	if err := e.WriteInt32([][]int32{make([]int32, 4), make([]int32, 3)}); err == nil {
		t.Error("accepted channels of differing lengths")
	}
}

func TestFloatToIntegerConversionClamps(t *testing.T) {
	var buf seekableBuffer
	e, err := NewEncoder(&buf, Format{SampleRate: 44100, Channels: 1, BitsPerSample: 16})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	if err := e.WriteFloat32([][]float32{{-4, -1, 0, 1, 4}}); err != nil {
		t.Fatalf("WriteFloat32: %v", err)
	}
	_ = e.Close()

	got, _, err := DecodeAllInt32(bytes.NewReader(buf.data))
	if err != nil {
		t.Fatalf("DecodeAllInt32: %v", err)
	}
	want := []int32{-32768, -32768, 0, 32767, 32767}
	requireEqual(t, got, [][]int32{want})
}

func FuzzDecode(f *testing.F) {
	f.Add(encodeIntsRaw())
	f.Add([]byte("RIFF"))
	f.Add([]byte("RIFF\x00\x00\x00\x00WAVE"))

	f.Fuzz(func(t *testing.T, data []byte) {
		samples, format, err := DecodeAllInt32(bytes.NewReader(data))
		if err != nil {
			return
		}
		if len(samples) != format.Channels {
			t.Fatalf("decoded %d channels, format declares %d", len(samples), format.Channels)
		}
		for i := 1; i < len(samples); i++ {
			if len(samples[i]) != len(samples[0]) {
				t.Fatalf("channel %d has %d frames, channel 0 has %d",
					i, len(samples[i]), len(samples[0]))
			}
		}
	})
}

// encodeIntsRaw makes a valid seed without a *testing.T.
func encodeIntsRaw() []byte {
	var buf seekableBuffer
	e, err := NewEncoder(&buf, Format{SampleRate: 44100, Channels: 2, BitsPerSample: 16})
	if err != nil {
		return nil
	}
	_ = e.WriteInt32([][]int32{{1, 2, 3}, {4, 5, 6}})
	_ = e.Close()
	return buf.data
}

func BenchmarkWriteInt32(b *testing.B) {
	const n = 44100
	samples := [][]int32{make([]int32, n), make([]int32, n)}
	for ch := range samples {
		for i := range n {
			samples[ch][i] = int32(10000 * math.Sin(float64(i)*0.01))
		}
	}
	b.SetBytes(int64(n * 2 * 2))
	for b.Loop() {
		var buf seekableBuffer
		e, _ := NewEncoder(&buf, Format{SampleRate: 44100, Channels: 2, BitsPerSample: 16})
		_ = e.WriteInt32(samples)
		_ = e.Close()
	}
}

// TestFormatChunkSizeIsNotTrusted is a regression test for a bug the fuzzer found.
//
// The chunk size field is four attacker-controlled bytes, and the reader allocated that many before
// checking whether the data existed. A file whose "fmt " length happened to read as the bytes "data"
// asked for a 1.6 GB allocation. Only as much of the chunk as carries meaning is held now, and the
// rest is skipped.
func TestFormatChunkSizeIsNotTrusted(t *testing.T) {
	// "RIFF" ... "WAVE" then a fmt chunk whose size field reads as "data".
	data := []byte("RIFF0\x00\x00\x00WAVEfmt data\xf9\xff\xff\xff\x00\x00\x04\x00\x02\x00\x05\x00\x03\x00\x06a")
	if _, _, err := DecodeAllInt32(bytes.NewReader(data)); err == nil {
		t.Error("accepted a format chunk with an impossible length")
	}

	// The same shape at a scale that would exhaust memory if it were allocated.
	huge := []byte("RIFF")
	huge = append(huge, 0xFF, 0xFF, 0xFF, 0xFF)
	huge = append(huge, "WAVEfmt "...)
	huge = append(huge, 0xFF, 0xFF, 0xFF, 0x7F) // two gigabytes
	if _, _, err := DecodeAllInt32(bytes.NewReader(huge)); err == nil {
		t.Error("accepted a two gigabyte format chunk")
	}
}
