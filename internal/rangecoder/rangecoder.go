// Package rangecoder implements the Opus entropy coder of RFC 6716 section 4.1 and 5.1.
//
// It is a range coder rather than a bit packer, and it is unusual in reading from both ends of a
// frame at once: entropy-coded symbols come from the front, most significant bit first, while "raw
// bits" come from the back, least significant bit first. The two are allowed to overlap in the
// middle, which is why neither can be built on the readers in internal/bitio.
//
// Every operation is bit-exact integer arithmetic. A range coder that is close is a range coder that
// desynchronises, so there is no tolerance anywhere in here.
package rangecoder

import "math/bits"

// Constants from the reference formulation in RFC 6716 section 4.1.
const (
	codeBits   = 32
	symBits    = 8
	symMax     = 255
	codeShift  = codeBits - symBits - 1 // 23
	codeTop    = uint32(1) << (codeBits - 1)
	codeBot    = uint32(1) << codeShift
	codeExtra  = (codeBits-2)%symBits + 1 // 7
	uintBits   = 8
	windowSize = 32
)

// Decoder reads symbols and raw bits from one Opus frame.
type Decoder struct {
	data []byte

	offs    int    // bytes consumed from the front
	endOffs int    // bytes consumed from the back
	val     uint32 // distance from the top of the range to the coded value, less one
	rng     uint32 // size of the current range
	rem     int    // the byte last read from the front, whose low bit carries over
	ext     uint32 // rng/ft from the most recent symbol lookup

	endWindow uint32 // raw bits buffered from the back
	nendBits  int

	nbitsTotal int
}

// NewDecoder starts a decoder over one frame.
func NewDecoder(data []byte) *Decoder {
	d := &Decoder{
		data: data,
		rng:  1 << codeExtra,
		// Counts the bits buffered inside the range coder as well as those consumed, so it is an
		// upper bound rather than an exact figure.
		//
		// It starts below the eventual offset because the initial renormalisation adds the bits it
		// reads; the two together reach codeBits+1, which is what makes a fresh decoder report the
		// single bit reserved for terminating the encoder.
		nbitsTotal: codeBits + 1 - ((codeBits-codeExtra)/symBits)*symBits,
	}
	d.rem = int(d.readByte())
	d.val = d.rng - 1 - uint32(d.rem)>>(symBits-codeExtra)
	d.normalize()
	return d
}

// readByte takes the next byte from the front, yielding zero past the end.
//
// RFC 6716 section 4.1.2.1: once the frame is exhausted the decoder must keep supplying zeros, even
// when the packet holds more data from padding or another frame.
func (d *Decoder) readByte() byte {
	if d.offs >= len(d.data) {
		return 0
	}
	b := d.data[d.offs]
	d.offs++
	return b
}

// readByteFromEnd takes the next byte from the back, yielding zero past the end.
func (d *Decoder) readByteFromEnd() byte {
	if d.endOffs >= len(d.data) {
		return 0
	}
	d.endOffs++
	return d.data[len(d.data)-d.endOffs]
}

// normalize refills the range until it exceeds 2^23.
func (d *Decoder) normalize() {
	for d.rng <= codeBot {
		d.nbitsTotal += symBits
		d.rng <<= symBits

		// The high bit of sym is the bit left over from the previous byte; the rest are the top
		// seven bits of the byte just read.
		sym := d.rem
		d.rem = int(d.readByte())
		sym = ((sym<<symBits | d.rem) >> (symBits - codeExtra)) & symMax

		d.val = ((d.val << symBits) + uint32(symMax-sym)) & (codeTop - 1)
	}
}

// Decode returns a value fs inside the range of some symbol in a context of total frequency ft.
//
// The caller maps fs onto a symbol and then calls Update with that symbol's frequencies.
func (d *Decoder) Decode(ft uint32) uint32 {
	d.ext = d.rng / ft
	s := d.val / d.ext
	if s+1 < ft {
		return ft - (s + 1)
	}
	return 0
}

// DecodeBin is Decode for a context whose total frequency is a power of two.
//
// It is equivalent to Decode(1<<ftb) and avoids one division, which matters because CELT calls it
// for every Laplace-coded value.
func (d *Decoder) DecodeBin(ftb uint) uint32 {
	d.ext = d.rng >> ftb
	s := d.val / d.ext
	ft := uint32(1) << ftb
	if s+1 < ft {
		return ft - (s + 1)
	}
	return 0
}

// Update advances the decoder past a symbol whose frequencies are fl, fh out of ft.
func (d *Decoder) Update(fl, fh, ft uint32) {
	s := d.ext * (ft - fh)
	d.val -= s
	if fl > 0 {
		d.rng = d.ext * (fh - fl)
	} else {
		d.rng -= s
	}
	d.normalize()
}

// DecodeSymbol reads one symbol from a context given as cumulative frequencies.
//
// cdf[0] must be zero and cdf[len-1] is the total frequency, so the symbol count is len(cdf)-1.
func (d *Decoder) DecodeSymbol(cdf []uint32) int {
	ft := cdf[len(cdf)-1]
	fs := d.Decode(ft)

	k := 0
	for k < len(cdf)-2 && cdf[k+1] <= fs {
		k++
	}
	d.Update(cdf[k], cdf[k+1], ft)
	return k
}

// DecodeICDF reads a symbol from a context given as an inverse cumulative distribution.
//
// icdf holds the complement of the cumulative frequencies, scaled to 1<<ftb and ending at zero. The
// form lets the search walk downward and stop on a comparison, avoiding the division Decode needs.
func (d *Decoder) DecodeICDF(icdf []byte, ftb uint) int {
	s := d.rng
	dv := d.val
	r := s >> ftb

	ret := -1
	var t uint32
	for {
		t = s
		ret++
		s = r * uint32(icdf[ret])
		if dv >= s {
			break
		}
	}
	d.val = dv - s
	d.rng = t - s
	d.normalize()
	return ret
}

// DecodeBitLogp reads a single bit whose probability of being zero is 1 - 2^-logp.
func (d *Decoder) DecodeBitLogp(logp uint32) int {
	r := d.rng
	dv := d.val
	s := r >> logp

	ret := 0
	if dv < s {
		ret = 1
	}
	if ret != 0 {
		d.rng = s
	} else {
		d.val = dv - s
		d.rng = r - s
	}
	d.normalize()
	return ret
}

// DecodeBits reads bits packed at the end of the frame, least significant bit first.
func (d *Decoder) DecodeBits(n uint) uint32 {
	window := d.endWindow
	available := d.nendBits

	if available < int(n) {
		for available <= windowSize-symBits {
			window |= uint32(d.readByteFromEnd()) << available
			available += symBits
		}
	}

	ret := window & (1<<n - 1)
	window >>= n
	available -= int(n)

	d.endWindow = window
	d.nendBits = available
	d.nbitsTotal += int(n)
	return ret
}

// DecodeUint reads one of ft equiprobable values in [0, ft).
//
// Values needing more than eight bits are split: the high bits go through the range coder and the
// remainder comes from raw bits, which bounds the number of divisions in the worst case.
func (d *Decoder) DecodeUint(ft uint32) uint32 {
	if ft <= 1 {
		return 0
	}
	limit := ft - 1
	ftb := ilog(limit)

	if ftb <= uintBits {
		s := d.Decode(ft)
		d.Update(s, s+1, ft)
		return s
	}

	ftb -= uintBits
	scaled := (limit >> ftb) + 1
	s := d.Decode(scaled)
	d.Update(s, s+1, scaled)

	t := s<<ftb | d.DecodeBits(uint(ftb))
	if t > limit {
		// RFC 6716 section 4.1.5: a value past the range means the frame is corrupt. Saturating is
		// the concealment the specification suggests.
		return limit
	}
	return t
}

// Tell reports an upper bound on the bits consumed so far, to whole-bit precision.
//
// A freshly initialised decoder reports one bit, which is the bit reserved for terminating the
// encoder.
func (d *Decoder) Tell() int {
	return d.nbitsTotal - int(ilog(d.rng))
}

// TellFrac reports the bits consumed to eighth-bit precision.
func (d *Decoder) TellFrac() int {
	return tellFrac(d.nbitsTotal, d.rng)
}

// Range returns the coder's current range, which an encoder decoding the same symbols must match.
//
// RFC 6716 section 5.1 calls this out as a tool for finding faults in either side, because the two
// values agree only if every symbol so far was handled identically.
func (d *Decoder) Range() uint32 { return d.rng }

// ilog returns the number of bits needed to hold v, and zero for zero.
func ilog(v uint32) uint32 { return uint32(bits.Len32(v)) }

// tellFrac estimates buffered bits to eighth-bit precision. RFC 6716 section 4.1.6.2.
func tellFrac(nbitsTotal int, rng uint32) int {
	// Correction table for the fractional part, from the reference formulation.
	var correction = [8]uint32{35733, 38967, 42495, 46340, 50535, 55109, 60097, 65535}

	lg := ilog(rng)
	r := rng >> (lg - 16)

	b := (r >> 12) - 8
	if r > correction[b] {
		b++
	}
	l := lg*8 + b
	return nbitsTotal*8 - int(l)
}
