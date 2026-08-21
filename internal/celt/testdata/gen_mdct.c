/* Oracle for the CELT inverse MDCT, built against the RFC 6716 reference.
   Emits vectors consumed by TestConformanceInverseMDCTMatchesReference.

   Build (from the extracted reference root):
     gcc -O2 -I celt -I include -o gen gen_mdct.c celt/mdct.c celt/kiss_fft.c celt/modes.c \
         celt/entcode.c celt/mathops.c -lm
*/
#include <stdio.h>
#include <stdlib.h>
#include "modes.h"
#include "mdct.h"

/* A cheap deterministic source, so the Go side can rebuild the same input. */
static unsigned s;
static float nextval(void) {
   s = 1664525u * s + 1013904223u;
   return (float)((int)(s >> 8) - 8388608) / 8388608.0f;
}

int main(void)
{
   int err;
   const CELTMode *m = opus_custom_mode_create(48000, 960, &err);
   if (!m) { fprintf(stderr, "mode create failed\n"); return 1; }

   printf("# overlap %d shortMdctSize %d maxLM %d\n",
          m->overlap, m->shortMdctSize, m->maxLM);
   for (int i = 0; i < m->overlap; i++)
      printf("window %d %.9g\n", i, (double)m->window[i]);

   /* One case per frame size, long blocks and short. */
   for (int LM = 0; LM <= 3; LM++) {
      for (int shortBlocks = 0; shortBlocks <= 1; shortBlocks++) {
         int N = m->shortMdctSize << LM;
         int overlap = m->overlap;
         int B = shortBlocks ? (1 << LM) : 1;
         int N2 = shortBlocks ? m->shortMdctSize : N;
         int shift = shortBlocks ? m->maxLM : m->maxLM - LM;

         float *X = calloc(N, sizeof(float));
         float *x = calloc(N + overlap, sizeof(float));
         s = 12345u + LM * 77u + shortBlocks * 999u;
         for (int i = 0; i < N; i++) X[i] = nextval();

         printf("case %d %d %d %d %d\n", LM, shortBlocks, N, B, shift);
         for (int i = 0; i < N; i++) printf("in %.9g\n", (double)X[i]);

         for (int i = 0; i < overlap; i++) x[i] = 0;
         for (int b = 0; b < B; b++)
            clt_mdct_backward(&m->mdct, &X[b], x + N2 * b, m->window, overlap, shift, B);

         for (int i = 0; i < N + overlap; i++) printf("out %.9g\n", (double)x[i]);
         free(X); free(x);
      }
   }
   opus_custom_mode_destroy((CELTMode *)m);
   return 0;
}
