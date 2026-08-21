package celt

import "github.com/faide/goxiph/internal/rangecoder"

// A frame can trade frequency resolution for time resolution band by band. A transient wants short
// blocks so the attack is not smeared, and a steady tone wants long ones; coding the choice per band
// lets one frame do both.
//
// Adapted from celt/celt.c, which RFC 6716 section 4.3.1 names without stating the procedure.

// tfSelectTable maps a frame size, whether the frame is transient, a per-frame selector and a
// per-band flag onto the resolution change for that band.
//
// The layout matters to a caller: the first four entries of each row are the non-transient ones and
// none of them is positive. Only a transient frame can merge blocks, and a transient frame is
// exactly the one that has blocks to merge. That is what keeps a change from asking for more blocks
// than the frame has.
var tfSelectTable = [4][8]int8{
	{0, -1, 0, -1, 0, -1, 0, -1},
	{0, -1, 0, -2, 1, 0, 1, -1},
	{0, -2, 0, -3, 2, 0, 1, -1},
	{0, -2, 0, -3, 3, 0, 1, -1},
}

// DecodeTFResolution reads the per-band time-frequency changes into tf.
//
// The flags are coded as a running exclusive-or so that a run of bands sharing a setting costs
// almost nothing, and the first band of a frame is cheaper to change than the rest.
func DecodeTFResolution(d *rangecoder.Decoder, tf []int, start, end, lm int, transient bool, totalBits int) {
	budget := totalBits
	tell := d.Tell()

	logp := uint32(4)
	if transient {
		logp = 2
	}

	// One bit is held back for the frame-wide selector, if there is room for it.
	selectReserved := lm > 0 && tell+int(logp)+1 <= budget
	if selectReserved {
		budget--
	}

	curr, changed := 0, 0
	for i := start; i < end; i++ {
		if tell+int(logp) <= budget {
			curr ^= d.DecodeBitLogp(logp)
			tell = d.Tell()
			changed |= curr
		}
		tf[i] = curr
		logp = 5
		if transient {
			logp = 4
		}
	}

	base := 0
	if transient {
		base = 4
	}
	// The selector is only coded where it would change something.
	tfSelect := 0
	if selectReserved && tfSelectTable[lm][base+changed] != tfSelectTable[lm][base+2+changed] {
		tfSelect = d.DecodeBitLogp(1)
	}
	for i := start; i < end; i++ {
		tf[i] = int(tfSelectTable[lm][base+2*tfSelect+tf[i]])
	}
}
