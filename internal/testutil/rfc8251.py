"""Apply the RFC 8251 corrections to an extracted copy of the RFC 6716 reference.

The reference implementation frozen into RFC 6716 is the 2012 code, from before RFC 8251 corrected
it. Five of its seven corrections reach the output, so an oracle built from the frozen source
disagrees with a decoder that implements both documents — most visibly on hybrid frames, which the
folding change rewrites. This makes a corrected copy to build oracles against.

Run: python3 internal/testutil/rfc8251.py <extracted-reference> <destination>
"""

import pathlib
import shutil
import sys


def apply(dst: pathlib.Path) -> None:
    # Section 9: hybrid folding. The first band is duplicated so that the second can fold onto it
    # rather than falling back to the noise generator.
    p = dst / "celt/bands.c"
    t = p.read_text()
    old = (
        "      if (resynth && M*eBands[i]-N >= M*eBands[start] && "
        "(update_lowband || lowband_offset==0))\n            lowband_offset = i;\n"
    )
    new = (
        "      if (resynth && (M*eBands[i]-N >= M*eBands[start] || i==start+1) && "
        "(update_lowband || lowband_offset==0))\n            lowband_offset = i;\n"
        "\n"
        "      if (i == start+1)\n"
        "      {\n"
        "         int n1, n2, offset;\n"
        "         n1 = M*(eBands[start+1]-eBands[start]);\n"
        "         n2 = M*(eBands[start+2]-eBands[start+1]);\n"
        "         offset = M*eBands[start];\n"
        "         OPUS_COPY(&norm[offset+n1], &norm[offset+2*n1 - n2], n2-n1);\n"
        "         if (C==2)\n"
        "            OPUS_COPY(&norm2[offset+n1], &norm2[offset+2*n1 - n2], n2-n1);\n"
        "      }\n"
    )
    assert old in t, "section 9 anchor"
    t = t.replace(old, new)
    old = "         while(M*eBands[++fold_end] < effective_lowband+N);"
    assert old in t, "section 9 fold_end anchor"
    t = t.replace(old, "         while(++fold_end < i && M*eBands[fold_end] < effective_lowband+N);")
    p.write_text(t)

    # Section 8: an extreme stream can name a log energy whose linear value is past what a float
    # holds, and the band would come out as not-a-number.
    p = dst / "celt/quant_bands.c"
    t = p.read_text()
    old = (
        "         opus_val16 lg = ADD16(oldEBands[i+c*m->nbEBands],\n"
        "                         SHL16((opus_val16)eMeans[i],6));\n"
        "         eBands[i+c*m->nbEBands] = PSHR32(celt_exp2(lg),4);"
    )
    assert old in t, "section 8 anchor"
    t = t.replace(
        old,
        "         opus_val16 lg = ADD16(oldEBands[i+c*m->nbEBands],\n"
        "                         SHL16((opus_val16)eMeans[i],6));\n"
        "         lg = MIN32(QCONST32(32.f, 16), lg);\n"
        "         eBands[i+c*m->nbEBands] = PSHR32(celt_exp2(lg),4);",
    )
    p.write_text(t)

    # Section 3: the stereo state has to be cleared with the rest of the decoder.
    p = dst / "silk/dec_API.c"
    t = p.read_text()
    i = t.index("opus_int silk_InitDecoder(")
    j = t.index("\n}", i)
    t = (
        t[:j]
        + "\n    silk_memset(&((silk_decoder *)decState)->sStereo, 0,\n"
        "                sizeof(((silk_decoder *)decState)->sStereo));\n"
        "    ((silk_decoder *)decState)->prev_decode_only_middle = 0;"
        + t[j:]
    )
    p.write_text(t)


def main() -> None:
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        raise SystemExit(2)
    src, dst = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
    if dst.exists():
        shutil.rmtree(dst)
    shutil.copytree(src, dst)
    apply(dst)
    print(f"rfc8251: corrected copy in {dst}")


if __name__ == "__main__":
    main()
