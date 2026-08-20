package flac

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"hash"
	"io"
	"math"
	"math/bits"

	"github.com/faide/goxiph/internal/bitio"
)

// DefaultBlockSize is the frame length the encoder uses unless told otherwise.
const DefaultBlockSize = 4096

// Rice parameter limits. Each coding method reserves its all-ones value as the escape code, so the
// widest usable parameter is one below.
const (
	maxRiceParam4 = 14
	maxRiceParam5 = 30
)

// maxPartitionOrder bounds the partition search. Higher orders cost a parameter per partition and
// stop paying for themselves well before the format's limit of 15.
const maxPartitionOrder = 8

// DefaultMaxLPCOrder is the widest linear predictor the encoder searches unless told otherwise.
//
// Eight captures most of the gain on real music while keeping the search cheap; the format permits
// up to 32.
const DefaultMaxLPCOrder = 8

// EncoderOptions configures an Encoder. The zero value is usable.
type EncoderOptions struct {
	// BlockSize is the frame length in samples. Zero selects DefaultBlockSize.
	BlockSize int

	// MaxLPCOrder bounds the linear predictor search. Zero selects DefaultMaxLPCOrder; a negative
	// value disables linear prediction, leaving the fixed predictors.
	MaxLPCOrder int
}

// Encoder writes a native FLAC stream.
//
// An Encoder is not safe for concurrent use.
type Encoder struct {
	w         io.Writer
	seeker    io.WriteSeeker // non-nil when the stream info block can be patched at Close
	info      StreamInfo
	blockSize int

	bw      *bitio.MSBWriter
	pending [][]int32

	frameNumber  uint64
	totalSamples uint64
	digest       hash.Hash
	digestBuf    []byte

	infoOffset         int64
	minFrame, maxFrame int

	// Per-block scratch, allocated once.
	coded    [][]int32
	residual []int32
	lpc      *lpcState
	closed   bool
}

// NewEncoder writes a FLAC stream describing info to w.
//
// TotalSamples and MD5 in info are ignored: both are computed while encoding. When w is an
// io.WriteSeeker they are patched into the stream info block at Close, and otherwise left as the
// zero the format defines to mean "unknown".
func NewEncoder(w io.Writer, info StreamInfo, opt EncoderOptions) (*Encoder, error) {
	blockSize := opt.BlockSize
	if blockSize == 0 {
		blockSize = DefaultBlockSize
	}
	if blockSize < 16 || blockSize > 65535 {
		return nil, fmt.Errorf("%w: block size %d outside 16..65535", ErrBadStream, blockSize)
	}
	if info.Channels < 1 || info.Channels > 8 {
		return nil, fmt.Errorf("%w: %d channels, want 1..8", ErrBadStream, info.Channels)
	}
	if info.BitsPerSample < 4 || info.BitsPerSample > 32 {
		return nil, fmt.Errorf("%w: %d bits per sample, want 4..32", ErrBadStream, info.BitsPerSample)
	}
	if info.SampleRate <= 0 {
		return nil, fmt.Errorf("%w: sample rate %d", ErrBadStream, info.SampleRate)
	}

	e := &Encoder{
		w:         w,
		info:      info,
		blockSize: blockSize,
		bw:        bitio.NewMSBWriter(),
		digest:    md5.New(),
		minFrame:  math.MaxInt32,
	}
	if s, ok := w.(io.WriteSeeker); ok {
		e.seeker = s
	}

	e.pending = make([][]int32, info.Channels)
	e.coded = make([][]int32, info.Channels)
	for i := range info.Channels {
		e.pending[i] = make([]int32, 0, blockSize)
		e.coded[i] = make([]int32, blockSize)
	}
	e.residual = make([]int32, blockSize)
	maxOrder := opt.MaxLPCOrder
	if maxOrder == 0 {
		maxOrder = DefaultMaxLPCOrder
	}
	if maxOrder > 0 {
		e.lpc = newLPCState(blockSize, min(maxOrder, maxLPCOrder))
	}
	e.digestBuf = make([]byte, 0, blockSize*info.Channels*4)

	if err := e.writeHeader(); err != nil {
		return nil, err
	}
	return e, nil
}

// writeHeader emits the signature and a stream info block, leaving room to patch it at Close.
func (e *Encoder) writeHeader() error {
	if _, err := io.WriteString(e.w, Signature); err != nil {
		return err
	}

	// Metadata block header: last block, type 0, 34 bytes.
	if _, err := e.w.Write([]byte{0x80, 0, 0, 34}); err != nil {
		return err
	}
	if e.seeker != nil {
		pos, err := e.seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			return err
		}
		e.infoOffset = pos
	}

	_, err := e.w.Write(e.streamInfoBytes())
	return err
}

// streamInfoBytes serialises the stream info payload from the state gathered so far.
func (e *Encoder) streamInfoBytes() []byte {
	// Every frame but the last is exactly the configured block size, and the minimum in the stream
	// info block excludes the last frame. Equal bounds are what declares a fixed block size stream,
	// which matches the blocking strategy bit the frame headers carry.
	minB, maxB := e.blockSize, e.blockSize

	minF, maxF := e.minFrame, e.maxFrame
	if minF > maxF {
		minF, maxF = 0, 0
	}

	p := make([]byte, 34)
	binary.BigEndian.PutUint16(p[0:2], uint16(minB))
	binary.BigEndian.PutUint16(p[2:4], uint16(maxB))
	p[4], p[5], p[6] = byte(minF>>16), byte(minF>>8), byte(minF)
	p[7], p[8], p[9] = byte(maxF>>16), byte(maxF>>8), byte(maxF)

	packed := uint32(e.info.SampleRate)<<4 |
		uint32(e.info.Channels-1)<<1 |
		uint32(e.info.BitsPerSample-1)>>4
	p[10], p[11], p[12] = byte(packed>>16), byte(packed>>8), byte(packed)
	p[13] = byte(e.info.BitsPerSample-1)<<4 | byte(e.totalSamples>>32)
	binary.BigEndian.PutUint32(p[14:18], uint32(e.totalSamples))

	if e.closed {
		copy(p[18:34], e.digest.Sum(nil))
	}
	return p
}

// Write appends interleaved-by-channel samples, emitting frames as blocks fill.
//
// samples holds one slice per channel, all of the same length.
func (e *Encoder) Write(samples [][]int32) error {
	if e.closed {
		return fmt.Errorf("%w: write to a closed encoder", ErrBadStream)
	}
	if len(samples) != e.info.Channels {
		return fmt.Errorf("%w: %d channels, stream declares %d", ErrBadStream, len(samples), e.info.Channels)
	}
	if len(samples) == 0 {
		return nil
	}
	n := len(samples[0])
	for i, ch := range samples {
		if len(ch) != n {
			return fmt.Errorf("%w: channel %d has %d samples, channel 0 has %d", ErrBadStream, i, len(ch), n)
		}
	}

	for pos := 0; pos < n; {
		room := e.blockSize - len(e.pending[0])
		take := min(room, n-pos)
		for ch := range samples {
			e.pending[ch] = append(e.pending[ch], samples[ch][pos:pos+take]...)
		}
		pos += take

		if len(e.pending[0]) == e.blockSize {
			if err := e.flushBlock(); err != nil {
				return err
			}
		}
	}
	return nil
}

// Close writes any buffered samples and finalises the stream info block.
func (e *Encoder) Close() error {
	if e.closed {
		return nil
	}
	if len(e.pending) > 0 && len(e.pending[0]) > 0 {
		if err := e.flushBlock(); err != nil {
			return err
		}
	}
	e.closed = true

	if e.seeker == nil {
		// Total samples and the checksum stay zero, which the format defines as unknown.
		return nil
	}

	here, err := e.seeker.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if _, err := e.seeker.Seek(e.infoOffset, io.SeekStart); err != nil {
		return err
	}
	if _, err := e.seeker.Write(e.streamInfoBytes()); err != nil {
		return err
	}
	_, err = e.seeker.Seek(here, io.SeekStart)
	return err
}

// flushBlock encodes and writes one frame from the pending samples.
func (e *Encoder) flushBlock() error {
	n := len(e.pending[0])
	if n == 0 {
		return nil
	}

	e.hashBlock(n)

	e.bw.Reset()
	mode := e.decorrelate(n)
	if err := e.writeFrameHeader(n, mode); err != nil {
		return err
	}

	// The header checksum covers the header bytes written so far.
	header := e.bw.Bytes()
	_ = e.bw.Write(uint64(crc8(header)), 8)

	side := sideChannelFor(mode)
	for ch := range e.info.Channels {
		depth := uint(e.info.BitsPerSample)
		if ch == side {
			depth++
		}
		if err := e.writeSubframe(e.coded[ch][:n], depth); err != nil {
			return err
		}
	}

	e.bw.AlignByte()
	frame := e.bw.Bytes()
	_ = e.bw.Write(uint64(crc16(frame)), 16)

	out := e.bw.Bytes()
	if _, err := e.w.Write(out); err != nil {
		return err
	}

	e.minFrame = min(e.minFrame, len(out))
	e.maxFrame = max(e.maxFrame, len(out))
	e.totalSamples += uint64(n)
	e.frameNumber++

	for ch := range e.pending {
		e.pending[ch] = e.pending[ch][:0]
	}
	return nil
}

// hashBlock folds the block into the running MD5, which covers the samples as the decoder will
// produce them: interleaved, little-endian, sign-extended to whole bytes.
func (e *Encoder) hashBlock(n int) {
	width := (e.info.BitsPerSample + 7) / 8
	e.digestBuf = e.digestBuf[:0]
	for i := range n {
		for ch := range e.info.Channels {
			v := uint32(e.pending[ch][i])
			for b := range width {
				e.digestBuf = append(e.digestBuf, byte(v>>(8*b)))
			}
		}
	}
	e.digest.Write(e.digestBuf)
}

// sideChannelFor reports which coded channel carries the side signal, or -1.
func sideChannelFor(mode int) int {
	switch mode {
	case channelsLeftSide, channelsMidSide:
		return 1
	case channelsSideRight:
		return 0
	default:
		return -1
	}
}

// decorrelate picks a stereo mode and fills e.coded with the channels to encode.
//
// The four options are scored by the total magnitude of their first-order differences, which tracks
// the coded size closely enough to choose between them and costs one pass instead of four trial
// encodes.
func (e *Encoder) decorrelate(n int) int {
	if e.info.Channels != 2 {
		for ch := range e.info.Channels {
			copy(e.coded[ch][:n], e.pending[ch][:n])
		}
		return channelsIndependent
	}

	left, right := e.pending[0][:n], e.pending[1][:n]
	var sumL, sumR, sumM, sumS float64
	var prevL, prevR, prevM, prevS int64
	for i := range n {
		l, r := int64(left[i]), int64(right[i])
		m := (l + r) >> 1
		s := l - r
		sumL += absDiff(l, prevL)
		sumR += absDiff(r, prevR)
		sumM += absDiff(m, prevM)
		sumS += absDiff(s, prevS)
		prevL, prevR, prevM, prevS = l, r, m, s
	}

	// Estimated bits, not summed magnitudes: coded size grows with the logarithm of the residual
	// magnitude, so comparing sums directly misjudges pairs whose magnitudes differ widely. The side
	// channel is charged the one extra bit of depth it carries.
	estL, estR := estimateBits(sumL, n), estimateBits(sumR, n)
	estM, estS := estimateBits(sumM, n), estimateBits(sumS, n)+float64(n)

	best, mode := estL+estR, channelsIndependent
	if c := estL + estS; c < best {
		best, mode = c, channelsLeftSide
	}
	if c := estS + estR; c < best {
		best, mode = c, channelsSideRight
	}
	if c := estM + estS; c < best {
		mode = channelsMidSide
	}

	switch mode {
	case channelsLeftSide:
		for i := range n {
			e.coded[0][i] = left[i]
			e.coded[1][i] = left[i] - right[i]
		}
	case channelsSideRight:
		for i := range n {
			e.coded[0][i] = left[i] - right[i]
			e.coded[1][i] = right[i]
		}
	case channelsMidSide:
		for i := range n {
			e.coded[0][i] = int32((int64(left[i]) + int64(right[i])) >> 1)
			e.coded[1][i] = left[i] - right[i]
		}
	default:
		copy(e.coded[0][:n], left)
		copy(e.coded[1][:n], right)
	}
	return mode
}

// estimateBits approximates the coded size of a channel from the total magnitude of its first
// differences. A Rice code spends roughly log2 of the mean residual per sample, plus overhead.
func estimateBits(sum float64, n int) float64 {
	if n == 0 {
		return 0
	}
	mean := sum / float64(n)
	return float64(n) * math.Max(0, math.Log2(1+mean))
}

func absDiff(a, b int64) float64 {
	d := a - b
	if d < 0 {
		d = -d
	}
	return float64(d)
}

// writeFrameHeader emits the frame header. RFC 9639 section 9.1.
func (e *Encoder) writeFrameHeader(n, mode int) error {
	_ = e.bw.Write(frameSync, 15)
	_ = e.bw.Write(0, 1) // fixed block size stream, so the coded number is a frame number

	blockCode, blockExtra, blockExtraBits := encodeBlockSize(n)
	rateCode, rateExtra, rateExtraBits := encodeSampleRate(e.info.SampleRate)
	depthCode, ok := encodeBitDepth(e.info.BitsPerSample)
	if !ok {
		return fmt.Errorf("%w: %d bits per sample cannot be coded in a frame header",
			ErrBadStream, e.info.BitsPerSample)
	}

	channelCode := uint64(e.info.Channels - 1)
	if mode != channelsIndependent {
		channelCode = uint64(7 + mode)
	}

	_ = e.bw.Write(blockCode, 4)
	_ = e.bw.Write(rateCode, 4)
	_ = e.bw.Write(channelCode, 4)
	_ = e.bw.Write(depthCode, 3)
	_ = e.bw.Write(0, 1) // reserved

	writeCodedNumber(e.bw, e.frameNumber)

	if blockExtraBits > 0 {
		_ = e.bw.Write(blockExtra, blockExtraBits)
	}
	if rateExtraBits > 0 {
		_ = e.bw.Write(rateExtra, rateExtraBits)
	}
	return nil
}

// encodeBlockSize returns the block size code and any value stored after the coded number.
func encodeBlockSize(n int) (code, extra uint64, extraBits uint) {
	for i, v := range blockSizeTable {
		if v != 0 && v == n {
			return uint64(i), 0, 0
		}
	}
	if n <= 256 {
		return 6, uint64(n - 1), 8
	}
	return 7, uint64(n - 1), 16
}

// encodeSampleRate returns the sample rate code and any value stored after the block size.
func encodeSampleRate(rate int) (code, extra uint64, extraBits uint) {
	for i, v := range sampleRateTable {
		if v > 0 && v == rate {
			return uint64(i), 0, 0
		}
	}
	if rate%1000 == 0 && rate/1000 < 256 {
		return 12, uint64(rate / 1000), 8
	}
	if rate < 65536 {
		return 13, uint64(rate), 16
	}
	if rate%10 == 0 && rate/10 < 65536 {
		return 14, uint64(rate / 10), 16
	}
	// The rate cannot be coded in the frame header, so defer to the stream info block.
	return 0, 0, 0
}

func encodeBitDepth(depth int) (uint64, bool) {
	for i, v := range bitDepthTable {
		if v > 0 && v == depth {
			return uint64(i), true
		}
	}
	// Code 0 defers to the stream info block, which carries any depth the format allows.
	return 0, true
}

// writeCodedNumber emits the frame number in the UTF-8-like form of RFC 9639 section 9.1.5.
func writeCodedNumber(w *bitio.MSBWriter, v uint64) {
	if v < 0x80 {
		_ = w.Write(v, 8)
		return
	}

	// Pick the shortest form that holds v, then fill the continuation octets from the top.
	var octets int
	switch {
	case v < 0x800:
		octets = 2
	case v < 0x10000:
		octets = 3
	case v < 0x200000:
		octets = 4
	case v < 0x4000000:
		octets = 5
	case v < 0x80000000:
		octets = 6
	default:
		octets = 7
	}

	lead := byte(0xFF<<(8-octets)) & 0xFF
	payloadBits := uint(6 * (octets - 1))
	_ = w.Write(uint64(lead)|(v>>payloadBits), 8)
	for i := octets - 2; i >= 0; i-- {
		_ = w.Write(0x80|((v>>uint(6*i))&0x3F), 8)
	}
}

// writeSubframe encodes one channel, choosing the cheapest representation available.
func (e *Encoder) writeSubframe(samples []int32, depth uint) error {
	wasted := commonWastedBits(samples)
	if wasted > 0 {
		if wasted >= depth {
			// Every sample is zero; leave one bit of depth so the constant form stays valid.
			wasted = depth - 1
		}
		for i := range samples {
			samples[i] >>= wasted
		}
		depth -= wasted
	}

	writeHeader := func(kind uint64) {
		_ = e.bw.Write(0, 1)
		_ = e.bw.Write(kind, 6)
		if wasted == 0 {
			_ = e.bw.Write(0, 1)
			return
		}
		_ = e.bw.Write(1, 1)
		_ = e.bw.WriteUnary(int(wasted) - 1)
	}

	if isConstant(samples) {
		writeHeader(0)
		_ = e.bw.Write(uint64(samples[0])&mask(depth), depth)
		return nil
	}

	order, partOrder, plans, cost := e.bestFixedPredictor(samples, depth)
	best := e.bestLPCPredictor(samples, depth)
	verbatimCost := int(depth) * len(samples)

	if verbatimCost <= cost && (!best.valid || verbatimCost <= best.cost) {
		writeHeader(1)
		for _, v := range samples {
			_ = e.bw.Write(uint64(v)&mask(depth), depth)
		}
		return nil
	}

	if best.valid && best.cost < cost {
		writeHeader(uint64(31 + best.order))
		for i := range best.order {
			_ = e.bw.Write(uint64(samples[i])&mask(depth), depth)
		}
		_ = e.bw.Write(uint64(best.precision-1), 4)
		_ = e.bw.Write(uint64(best.shift)&0x1F, 5)
		for i := range best.order {
			_ = e.bw.Write(uint64(best.coeffs[i])&mask(best.precision), best.precision)
		}
		lpcResidual(samples, best.coeffs[:best.order], best.shift, e.residual[:len(samples)])
		e.writeResidual(e.residual[best.order:len(samples)], len(samples), best.order, best.partOrder, best.plans)
		return nil
	}

	writeHeader(uint64(8 + order))
	for i := range order {
		_ = e.bw.Write(uint64(samples[i])&mask(depth), depth)
	}
	fixedResidual(samples, order, e.residual[:len(samples)])
	e.writeResidual(e.residual[order:len(samples)], len(samples), order, partOrder, plans)
	return nil
}

// mask returns a bit mask of n bits, guarding the shift at n == 64.
func mask(n uint) uint64 {
	if n >= 64 {
		return ^uint64(0)
	}
	return 1<<n - 1
}

func isConstant(s []int32) bool {
	for _, v := range s[1:] {
		if v != s[0] {
			return false
		}
	}
	return true
}

// commonWastedBits counts the low zero bits every sample shares.
func commonWastedBits(s []int32) uint {
	var or uint32
	for _, v := range s {
		or |= uint32(v)
	}
	if or == 0 {
		return 0
	}
	return uint(bits.TrailingZeros32(or))
}

// fixedResidual writes the residual of a fixed predictor into dst, leaving dst[:order] untouched.
func fixedResidual(samples []int32, order int, dst []int32) {
	coeffs := fixedCoefficients[order]
	for i := order; i < len(samples); i++ {
		var pred int64
		for j, c := range coeffs {
			pred += c * int64(samples[i-1-j])
		}
		dst[i] = samples[i] - int32(pred)
	}
}

// bestFixedPredictor picks the fixed predictor order and partitioning with the smallest coded size.
func (e *Encoder) bestFixedPredictor(samples []int32, depth uint) (order, partOrder int, plans []partitionPlan, cost int) {
	bestCost := math.MaxInt
	for o := range 5 {
		if o >= len(samples) {
			break
		}
		fixedResidual(samples, o, e.residual[:len(samples)])
		po, ps, c := e.bestPartitioning(e.residual[o:len(samples)], len(samples), o)
		c += int(depth) * o // warm-up samples
		if c < bestCost {
			bestCost, order, partOrder, plans = c, o, po, ps
		}
	}
	return order, partOrder, plans, bestCost
}

// partitionPlan is how one partition will be coded.
type partitionPlan struct {
	escape bool
	param  uint // Rice parameter when escape is false
	width  uint // unencoded sample width when escape is true
	bits   int  // coded size including the parameter field
}

// bestPartitioning chooses the partition order and how each partition is coded.
func (e *Encoder) bestPartitioning(residual []int32, blockSize, order int) (int, []partitionPlan, int) {
	bestOrder, bestCost := 0, math.MaxInt
	var bestPlans []partitionPlan

	for po := range maxPartitionOrder + 1 {
		partitions := 1 << po
		if blockSize%partitions != 0 {
			continue
		}
		perPartition := blockSize / partitions
		if perPartition <= order {
			break
		}

		plans := make([]partitionPlan, partitions)
		total := 2 + 4 // coding method and partition order
		pos := 0
		for p := range partitions {
			count := perPartition
			if p == 0 {
				count -= order
			}
			plans[p] = planPartition(residual[pos : pos+count])
			total += plans[p].bits
			pos += count
		}

		if total < bestCost {
			bestOrder, bestCost, bestPlans = po, total, plans
		}
	}
	if bestPlans == nil {
		bestPlans = []partitionPlan{planPartition(residual)}
		bestCost = bestPlans[0].bits + 6
	}
	return bestOrder, bestPlans, bestCost
}

// planPartition picks the cheaper of a Rice code and an unencoded escape for one partition.
//
// The escape is what keeps high-entropy residuals bounded: a Rice code whose parameter is too small
// for its data spends its unary part without limit, and for full-scale 24-bit samples that runs to
// millions of bits per sample.
func planPartition(residual []int32) partitionPlan {
	riceK, riceBits := bestRiceParam(residual)
	rice := partitionPlan{param: riceK, bits: riceBits + 4}

	width := escapeWidth(residual)
	escape := partitionPlan{escape: true, width: width, bits: 4 + 5 + int(width)*len(residual)}

	if escape.bits < rice.bits {
		return escape
	}
	return rice
}

// escapeWidth is the narrowest signed width holding every residual in a partition.
func escapeWidth(residual []int32) uint {
	var w uint
	for _, v := range residual {
		w = max(w, signedBits(v))
	}
	return w
}

// signedBits is the number of bits a two's complement representation of v needs, zero for zero.
func signedBits(v int32) uint {
	if v == 0 {
		return 0
	}
	// Folding the sign in means a negative value needs as many bits as its complement.
	return uint(bits.Len32(uint32(v^(v>>31)))) + 1
}

// bestRiceParam returns the parameter minimising the coded size of one partition.
//
// The optimum sits near log2 of the mean folded value, so the exact cost is evaluated over a small
// window around that estimate rather than across the whole range.
func bestRiceParam(residual []int32) (uint, int) {
	if len(residual) == 0 {
		return 0, 0
	}

	var sum uint64
	for _, v := range residual {
		sum += uint64(fold(v))
	}
	mean := sum / uint64(len(residual))

	estimate := 0
	if mean > 0 {
		estimate = bits.Len64(mean)
	}
	lo := max(0, estimate-2)
	hi := min(maxRiceParam5, estimate+2)

	bestK, bestBits := uint(lo), math.MaxInt
	for k := lo; k <= hi; k++ {
		total := 0
		for _, v := range residual {
			total += int(fold(v)>>uint(k)) + 1 + k
			if total < 0 {
				total = math.MaxInt // overflowed, so this parameter is hopeless
				break
			}
		}
		if total < bestBits {
			bestK, bestBits = uint(k), total
		}
	}
	return bestK, bestBits
}

// fold applies the zigzag mapping the Rice coder uses for signed values: a positive value doubles
// and a negative one doubles and steps down, so the sign needs no bit of its own.
func fold(v int32) uint32 {
	if v < 0 {
		return uint32(-v)<<1 - 1
	}
	return uint32(v) << 1
}

// writeResidual emits the partitioned residual, choosing the coding method that fits its parameters.
func (e *Encoder) writeResidual(residual []int32, blockSize, order, partOrder int, plans []partitionPlan) {
	// The 4-bit form cannot express a parameter above 14, so a single wide partition promotes the
	// whole residual to the 5-bit form.
	fiveBit := false
	for _, pl := range plans {
		if !pl.escape && pl.param > maxRiceParam4 {
			fiveBit = true
			break
		}
	}

	paramBits := uint(4)
	escapeCode := uint64(0b1111)
	if fiveBit {
		paramBits = 5
		escapeCode = 0b11111
		_ = e.bw.Write(1, 2)
	} else {
		_ = e.bw.Write(0, 2)
	}
	_ = e.bw.Write(uint64(partOrder), 4)

	partitions := 1 << partOrder
	perPartition := blockSize / partitions
	pos := 0
	for p := range partitions {
		count := perPartition
		if p == 0 {
			count -= order
		}
		pl := plans[min(p, len(plans)-1)]
		part := residual[pos : pos+count]
		pos += count

		if pl.escape {
			_ = e.bw.Write(escapeCode, paramBits)
			_ = e.bw.Write(uint64(pl.width), 5)
			if pl.width > 0 {
				for _, v := range part {
					_ = e.bw.Write(uint64(v)&mask(pl.width), pl.width)
				}
			}
			continue
		}

		_ = e.bw.Write(uint64(pl.param), paramBits)
		for _, v := range part {
			f := fold(v)
			_ = e.bw.WriteUnary(int(f >> pl.param))
			_ = e.bw.Write(uint64(f&uint32(mask(pl.param))), pl.param)
		}
	}
}
