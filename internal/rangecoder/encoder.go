package rangecoder

// Encoder writes symbols and raw bits into one Opus frame.
//
// It exists so the decoder can be checked by round trip. RFC 6716 defines both halves and states
// that the two must agree on the range after every symbol, which is the strongest available test
// short of the official vectors.
type Encoder struct {
	buf     []byte
	storage int

	offs    int
	endOffs int
	val     uint32
	rng     uint32

	// rem holds one output byte that cannot yet be emitted because a later carry may increment it,
	// and ext counts the 0xFF bytes a carry would propagate through. RFC 6716 section 5.1.1.2.
	rem int
	ext uint32

	endWindow  uint32
	nendBits   int
	nbitsTotal int

	overflow bool
}

// NewEncoder returns an encoder writing into a frame of size bytes.
func NewEncoder(size int) *Encoder {
	return &Encoder{
		buf:        make([]byte, size),
		storage:    size,
		rng:        codeTop,
		rem:        -1,
		nbitsTotal: codeBits + 1,
	}
}

func (e *Encoder) writeByte(b byte) {
	if e.offs+e.endOffs >= e.storage {
		e.overflow = true
		return
	}
	e.buf[e.offs] = b
	e.offs++
}

func (e *Encoder) writeByteAtEnd(b byte) {
	if e.offs+e.endOffs >= e.storage {
		e.overflow = true
		return
	}
	e.endOffs++
	e.buf[e.storage-e.endOffs] = b
}

// carryOut takes a nine-bit value: eight data bits and a carry.
func (e *Encoder) carryOut(c int) {
	if c == symMax {
		// A byte of 0xFF cannot absorb a carry, so it is counted rather than written; a later carry
		// will turn every one of them over at once.
		e.ext++
		return
	}

	carry := byte(c >> symBits)
	if e.rem >= 0 {
		e.writeByte(byte(e.rem) + carry)
	}
	if e.ext > 0 {
		sym := byte((symMax + int(carry)) & symMax)
		for ; e.ext > 0; e.ext-- {
			e.writeByte(sym)
		}
	}
	e.rem = c & symMax
}

func (e *Encoder) normalize() {
	for e.rng <= codeBot {
		e.carryOut(int(e.val >> codeShift))
		e.val = (e.val << symBits) & (codeTop - 1)
		e.rng <<= symBits
		e.nbitsTotal += symBits
	}
}

// Encode writes the symbol whose frequencies are fl, fh out of ft.
func (e *Encoder) Encode(fl, fh, ft uint32) {
	r := e.rng / ft
	if fl > 0 {
		e.val += e.rng - r*(ft-fl)
		e.rng = r * (fh - fl)
	} else {
		e.rng -= r * (ft - fh)
	}
	e.normalize()
}

// EncodeSymbol writes symbol k from a context given as cumulative frequencies.
func (e *Encoder) EncodeSymbol(k int, cdf []uint32) {
	e.Encode(cdf[k], cdf[k+1], cdf[len(cdf)-1])
}

// EncodeBitLogp writes a single bit whose probability of being zero is 1 - 2^-logp.
func (e *Encoder) EncodeBitLogp(bit int, logp uint32) {
	r := e.rng
	l := e.val
	s := r >> logp
	r -= s

	if bit != 0 {
		e.val = l + r
		e.rng = s
	} else {
		e.rng = r
	}
	e.normalize()
}

// EncodeBits writes raw bits at the end of the frame, least significant bit first.
func (e *Encoder) EncodeBits(v uint32, n uint) {
	window := e.endWindow
	used := e.nendBits

	if used+int(n) > windowSize {
		for used >= symBits {
			e.writeByteAtEnd(byte(window))
			window >>= symBits
			used -= symBits
		}
	}

	window |= v << used
	used += int(n)

	e.endWindow = window
	e.nendBits = used
	e.nbitsTotal += int(n)
}

// EncodeUint writes one of ft equiprobable values in [0, ft).
func (e *Encoder) EncodeUint(v, ft uint32) {
	if ft <= 1 {
		return
	}
	limit := ft - 1
	ftb := ilog(limit)

	if ftb <= uintBits {
		e.Encode(v, v+1, ft)
		return
	}

	ftb -= uintBits
	scaled := (limit >> ftb) + 1
	high := v >> ftb
	e.Encode(high, high+1, scaled)
	e.EncodeBits(v&(1<<ftb-1), uint(ftb))
}

// Tell reports the bits written so far, matching what a decoder reading the same symbols reports.
func (e *Encoder) Tell() int { return e.nbitsTotal - int(ilog(e.rng)) }

// TellFrac reports the bits written to eighth-bit precision.
func (e *Encoder) TellFrac() int { return tellFrac(e.nbitsTotal, e.rng) }

// Range returns the coder's current range, which must match the decoder's after the same symbols.
func (e *Encoder) Range() uint32 { return e.rng }

// Done finalises the frame and returns it.
//
// RFC 6716 section 5.1.5: the terminating value is chosen with as many trailing zero bits as the
// range allows, so those bits can carry raw data without disturbing the symbols already written.
func (e *Encoder) Done() []byte {
	l := int(codeBits - ilog(e.rng))
	msk := (codeTop - 1) >> uint(l)
	end := (e.val + msk) &^ msk

	if end|msk >= e.val+e.rng {
		l++
		msk >>= 1
		end = (e.val + msk) &^ msk
	}

	for l > 0 {
		e.carryOut(int(end >> codeShift))
		end = (end << symBits) & (codeTop - 1)
		l -= symBits
	}

	// Flush the buffered byte and any carry-propagating run behind it.
	if e.rem >= 0 || e.ext > 0 {
		e.carryOut(0)
	}

	window := e.endWindow
	used := e.nendBits
	for used >= symBits {
		e.writeByteAtEnd(byte(window))
		window >>= symBits
		used -= symBits
	}

	// Clear the gap between the two ends, then merge any leftover raw bits into the last byte the
	// range coder wrote, which is where they belong.
	if !e.overflow {
		clear(e.buf[e.offs : e.storage-e.endOffs])
		if used > 0 {
			if e.endOffs >= e.storage {
				e.overflow = true
			} else {
				if e.offs+e.endOffs >= e.storage && -l < used {
					window &= 1<<uint(-l) - 1
					e.overflow = true
				}
				e.buf[e.storage-e.endOffs-1] |= byte(window)
			}
		}
	}
	return e.buf
}

// Overflowed reports whether the frame ran out of room, which makes the output unusable.
func (e *Encoder) Overflowed() bool { return e.overflow }
