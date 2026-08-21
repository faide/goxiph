/* Oracle for the stages of transform-codec concealment, built against the RFC 6716 reference.

   celt_decode_lost works on state held inside an opaque decoder, so this mirrors its pitch-based
   branch on a buffer supplied here instead. Every intermediate is printed, which is what lets a
   difference be traced to a stage rather than merely observed at the output.

   Build (from the extracted reference root) and run:
     gcc -O2 -DVAR_ARRAYS -DCUSTOM_MODES -DOPUS_BUILD -w -I celt -I include \
         -o gen gen_conceal.c $(ls celt/*.c|grep -v demo) -lm
     ./gen > plc_stages.txt
*/
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <math.h>
#include "celt.h"
#include "pitch.h"
#include "celt_lpc.h"
#include "stack_alloc.h"
#include "modes.h"
#include "mathops.h"

#define DECODE_BUFFER_SIZE 2048
#define MAX_PERIOD 1024

static void dump(const char *name, const opus_val32 *v, int n)
{
   printf("%s %d", name, n);
   for (int i = 0; i < n; i++) printf(" %.9g", (double)v[i]);
   printf("\n");
}

int main(void)
{
   int err;
   const CELTMode *m = opus_custom_mode_create(48000, 960, &err);
   if (!m) { fprintf(stderr, "mode create failed\n"); return 1; }
   const int overlap = m->overlap;
   const int N = 960;
   const int C = 2;

   static celt_sig decode_mem[2][DECODE_BUFFER_SIZE + 128];
   /* The same signal the Go side builds: one period of noise repeated, plus a little fresh noise.
      A sine would do as well but for the fixture, whose two sides must agree to the last bit; this
      is built from a shift register and single-precision arithmetic, so they do. */
   static float base[137];
   unsigned s = 12345;
   for (int i = 0; i < 137; i++) {
      s = 1664525u * s + 1013904223u;
      base[i] = (float)((int)(s >> 8) - 8388608) / 8388608.0f;
   }
   for (int i = 0; i < DECODE_BUFFER_SIZE + overlap; i++) {
      s = 1664525u * s + 1013904223u;
      float n = (float)((int)(s >> 8) - 8388608) / 8388608.0f;
      decode_mem[0][i] = 0.4f * base[i % 137] + 0.05f * n;
      decode_mem[1][i] = 0.3f * base[(i + 40) % 137] + 0.05f * n;
   }

   celt_sig *dm[2] = { decode_mem[0], decode_mem[1] };
   celt_sig *out_mem[2] = { decode_mem[0] + DECODE_BUFFER_SIZE - MAX_PERIOD,
                            decode_mem[1] + DECODE_BUFFER_SIZE - MAX_PERIOD };

   int pitch_index;
   {
      opus_val16 lp[DECODE_BUFFER_SIZE >> 1];
      int poffset = 720;
      pitch_downsample(dm, lp, DECODE_BUFFER_SIZE, C);
      pitch_search(lp + (poffset >> 1), lp, DECODE_BUFFER_SIZE - poffset, poffset - 100, &pitch_index);
      pitch_index = poffset - pitch_index;
   }
   printf("pitch 1 %d\n", pitch_index);

   for (int c = 0; c < C; c++) {
      opus_val16 exc[MAX_PERIOD];
      opus_val32 ac[LPC_ORDER + 1];
      opus_val16 lpc[LPC_ORDER];
      opus_val16 mem[LPC_ORDER];
      opus_val32 e[MAX_PERIOD + 2 * 128];
      opus_val16 decay = 1;
      opus_val32 S1 = 0;
      int len = N + overlap;
      int offset;

      for (int i = 0; i < MAX_PERIOD; i++) exc[i] = out_mem[c][i];

      _celt_autocorr(exc, ac, m->window, overlap, LPC_ORDER, MAX_PERIOD);
      ac[0] *= 1.0001f;
      for (int i = 1; i <= LPC_ORDER; i++) ac[i] -= ac[i] * (.008f * i) * (.008f * i);
      _celt_lpc(lpc, ac, LPC_ORDER);
      printf("lpc%d %d", c, LPC_ORDER);
      for (int i = 0; i < LPC_ORDER; i++) printf(" %.9g", (double)lpc[i]);
      printf("\n");

      for (int i = 0; i < LPC_ORDER; i++) mem[i] = out_mem[c][MAX_PERIOD - 1 - i];
      celt_fir(exc, lpc, exc, MAX_PERIOD, LPC_ORDER, mem);
      printf("exc%d %d", c, MAX_PERIOD);
      for (int i = 0; i < MAX_PERIOD; i++) printf(" %.9g", (double)exc[i]);
      printf("\n");

      {
         opus_val32 E1 = 1, E2 = 1;
         int period = pitch_index <= MAX_PERIOD / 2 ? pitch_index : MAX_PERIOD / 2;
         for (int i = 0; i < period; i++) {
            E1 += exc[MAX_PERIOD - period + i] * exc[MAX_PERIOD - period + i];
            E2 += exc[MAX_PERIOD - 2 * period + i] * exc[MAX_PERIOD - 2 * period + i];
         }
         if (E1 > E2) E1 = E2;
         decay = celt_sqrt(frac_div32(E1, E2));
      }
      printf("decay%d 1 %.9g\n", c, (double)decay);

      offset = MAX_PERIOD - pitch_index;
      for (int i = 0; i < len + overlap; i++) {
         if (offset + i >= MAX_PERIOD) { offset -= pitch_index; decay = decay * decay; }
         e[i] = decay * exc[offset + i];
         opus_val16 t = out_mem[c][offset + i];
         S1 += t * t;
      }
      dump(c == 0 ? "ecopy0" : "ecopy1", e, len + overlap);
      printf("s1_%d 1 %.9g\n", c, (double)S1);

      for (int i = 0; i < LPC_ORDER; i++) mem[i] = out_mem[c][MAX_PERIOD - 1 - i];
      celt_iir(e, lpc, e, len + overlap, LPC_ORDER, mem);
      dump(c == 0 ? "eiir0" : "eiir1", e, len + overlap);
   }

   opus_custom_mode_destroy((CELTMode *)m);
   return 0;
}
