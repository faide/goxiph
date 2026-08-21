/* Oracle for CELT packet loss concealment, built against the RFC 6716 reference.

   Decodes a sequence of transform-codec frames and, after each, asks a copy of the decoder what it
   would play if the next packet never arrived. The copy matters: concealment advances the decoder's
   state, so asking on the live decoder would change what the following frame decodes to.

   Build (from the extracted reference root):
     gcc -O2 -DVAR_ARRAYS -DOPUS_BUILD -w -I celt -I silk -I silk/float -I include -I src \
         -o gen gen_plc.c $(ls celt/*.c|grep -v demo) silk/*.c silk/float/*.c \
         src/opus.c src/opus_encoder.c src/opus_decoder.c src/repacketizer.c -lm

   Run: gen <start-band> <end-band> <channels> <frame-samples> <frame-file>...
*/
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <math.h>
#include "celt.h"
#include "entdec.h"
#include "stack_alloc.h"
#include "pitch.h"
#include "celt_lpc.h"

int main(int argc, char **argv)
{
   if (argc < 6) { fprintf(stderr, "usage: gen <start> <end> <ch> <samples> <frame>...\n"); return 2; }
   int startBand = atoi(argv[1]), endBand = atoi(argv[2]);
   int channels = atoi(argv[3]), frameSamples = atoi(argv[4]);

   int err;
   int size = celt_decoder_get_size(channels);
   CELTDecoder *cd = malloc(size);
   err = celt_decoder_init(cd, 48000, channels);
   if (err != OPUS_OK) { fprintf(stderr, "celt: %s\n", opus_strerror(err)); return 1; }
   celt_decoder_ctl(cd, CELT_SET_START_BAND(startBand));
   celt_decoder_ctl(cd, CELT_SET_END_BAND(endBand));
   celt_decoder_ctl(cd, CELT_SET_CHANNELS(channels));

   /* A standalone check of the analysis concealment rests on, run on a known buffer so that the
      pitch and the filter fit can be compared without the rest of the decoder in the way. */
   if (startBand < 0) {
      const int len = 2048;
      static celt_sig buf0[2048], buf1[2048];
      celt_sig *ch[2] = { buf0, buf1 };
   /* The same signal the Go side builds: one period of noise repeated, plus a little fresh noise.
      A sine would do as well but for the fixture, whose two sides must agree to the last bit; this
      is built from a shift register and single-precision arithmetic, so they do. */
   static float base[137];
   unsigned s = 12345;
   for (int i = 0; i < 137; i++) {
      s = 1664525u * s + 1013904223u;
      base[i] = (float)((int)(s >> 8) - 8388608) / 8388608.0f;
   }
      for (int i = 0; i < len; i++) {
         s = 1664525u * s + 1013904223u;
         float n = (float)((int)(s >> 8) - 8388608) / 8388608.0f;
         buf0[i] = 0.4f * base[i % 137] + 0.05f * n;
         buf1[i] = 0.3f * base[(i + 40) % 137] + 0.05f * n;
      }
      opus_val16 lp[1024];
      pitch_downsample(ch, lp, len, channels);
      printf("downsampled %d", len / 2);
      for (int i = 0; i < len / 2; i++) printf(" %.9g", (double)lp[i]);
      printf("\n");

      int idx;
      pitch_search(lp + (720 >> 1), lp, len - 720, 720 - 100, &idx);
      printf("pitchindex 1 %d\n", idx);

      /* The fit on its own, run on the raw signal so that a difference here cannot be blamed on the
         decimation before it. */
      {
         opus_val16 x[512];
         for (int i = 0; i < 512; i++) x[i] = (opus_val16)buf0[i];
         opus_val32 a4[5];
         opus_val16 l4[4];
         _celt_autocorr(x, a4, NULL, 0, 4, 512);
         printf("ac4 5");
         for (int i = 0; i < 5; i++) printf(" %.9g", (double)a4[i]);
         printf("\n");
         _celt_lpc(l4, a4, 4);
         printf("lpc4 4");
         for (int i = 0; i < 4; i++) printf(" %.9g", (double)l4[i]);
         printf("\n");
      }

      opus_val32 ac[LPC_ORDER + 1];
      opus_val16 lpc[LPC_ORDER];
      _celt_autocorr(lp, ac, NULL, 0, LPC_ORDER, len / 2);
      ac[0] *= 1.0001f;
      for (int i = 1; i <= LPC_ORDER; i++) ac[i] -= ac[i] * (.008f * i) * (.008f * i);
      _celt_lpc(lpc, ac, LPC_ORDER);
      printf("lpc %d", LPC_ORDER);
      for (int i = 0; i < LPC_ORDER; i++) printf(" %.9g", (double)lpc[i]);
      printf("\n");
      return 0;
   }

   for (int a = 5; a < argc; a++) {
      FILE *f = fopen(argv[a], "rb");
      if (!f) { perror("open"); return 1; }
      static unsigned char buf[8192];
      int len = (int)fread(buf, 1, sizeof buf, f);
      fclose(f);

      opus_val16 pcm[1920 * 2];
      ec_dec dec;
      ec_dec_init(&dec, buf, len);
      celt_decode_with_ec(cd, buf, len, pcm, frameSamples, &dec);

      printf("frame %d\n", a - 5);
      printf("decoded %d", frameSamples * channels);
      for (int i = 0; i < frameSamples * channels; i++) printf(" %.9g", (double)pcm[i]);
      printf("\n");

      CELTDecoder *cc = malloc(size);
      memcpy(cc, cd, size);
      opus_val16 lost[1920 * 2];
      celt_decode_with_ec(cc, NULL, 0, lost, frameSamples, NULL);
      printf("concealed %d", frameSamples * channels);
      for (int i = 0; i < frameSamples * channels; i++) printf(" %.9g", (double)lost[i]);
      printf("\n");
      free(cc);
   }
   free(cd);
   return 0;
}
