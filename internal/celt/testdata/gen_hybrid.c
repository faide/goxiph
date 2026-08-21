/* Oracle for CELT decoding at a non-zero start band, which is how a hybrid packet uses it.

   The CELT-only gate exercises start band zero and nothing else, so a fault that depends on the
   start band survives it. This runs the reference through the hybrid arrangement — SILK first, then
   CELT from band seventeen on the same range decoder — and prints CELT's own output.

   Build (from the extracted reference root):
     gcc -O2 -DVAR_ARRAYS -DOPUS_BUILD -w -I celt -I silk -I silk/float -I include -I src \
         -o gen gen_hybrid.c $(ls celt/*.c|grep -v demo) silk/*.c silk/float/*.c \
         src/opus.c src/opus_encoder.c src/opus_decoder.c src/repacketizer.c -lm

   Run: gen <end-band> <channels> <frame-file>...

   The frames are decoded in sequence through one pair of decoders, because both carry state between
   frames and decoding each on its own would compare against a decoder that had forgotten.
*/
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "celt.h"
#include "entdec.h"
#include "main.h"
#include "stack_alloc.h"
#include "opus.h"

int main(int argc, char **argv)
{
   if (argc < 4) { fprintf(stderr, "usage: gen <endband> <channels> <frame>...\n"); return 2; }
   int endBand = atoi(argv[1]), channels = atoi(argv[2]);

   int err;
   CELTDecoder *cd = malloc(celt_decoder_get_size(channels));
   err = celt_decoder_init(cd, 48000, channels);
   if (err != OPUS_OK) { fprintf(stderr, "celt: %s\n", opus_strerror(err)); return 1; }
   celt_decoder_ctl(cd, CELT_SET_START_BAND(17));
   celt_decoder_ctl(cd, CELT_SET_END_BAND(endBand));
   celt_decoder_ctl(cd, CELT_SET_CHANNELS(channels));

   void *sd = malloc(100000);
   silk_InitDecoder(sd);

   /* The whole decoder as well, so the parts can be checked against what it produces. */
   int derr;
   OpusDecoder *od = opus_decoder_create(48000, channels, &derr);
   if (derr != OPUS_OK) { fprintf(stderr, "opus: %s\n", opus_strerror(derr)); return 1; }

   silk_DecControlStruct ctl;
   memset(&ctl, 0, sizeof ctl);
   ctl.nChannelsAPI = channels;
   ctl.nChannelsInternal = channels;
   ctl.API_sampleRate = 48000;
   ctl.internalSampleRate = 16000;
   ctl.payloadSize_ms = 20;

   for (int a = 3; a < argc; a++) {
      FILE *f = fopen(argv[a], "rb");
      if (!f) { perror("open"); return 1; }
      static unsigned char buf[8192];
      int len = (int)fread(buf, 1, sizeof buf, f);
      fclose(f);

      /* The packet carries a table-of-contents byte the codecs never see; only the whole decoder
         reads it. Code zero means one frame, which is all this handles. */
      unsigned char *frame = buf + 1;
      int frameLen = len - 1;

      ec_dec dec;
      ec_dec_init(&dec, frame, frameLen);

      opus_int16 silkOut[960 * 2];
      opus_int32 nOut = 0;
      silk_Decode(sd, &ctl, 0, 1, &dec, silkOut, &nOut);

      /* The flags that sit between the two codecs. */
      int usedLen = frameLen;
      if (ec_tell(&dec) + 37 <= 8 * frameLen) {
         if (ec_dec_bit_logp(&dec, 12)) {
            ec_dec_bit_logp(&dec, 1);
            int rb = (int)ec_dec_uint(&dec, 256) + 2;
            usedLen -= rb;
            dec.storage -= rb;
         }
      }

      opus_val16 pcm[960 * 2];
      celt_decode_with_ec(cd, frame, usedLen, pcm, 960, &dec);

      printf("frame %d\n", a - 3);
      printf("silk %d", (int)nOut * channels);
      for (int i = 0; i < nOut * channels; i++) printf(" %d", (int)silkOut[i]);
      printf("\n");
      printf("celt %d", 960 * channels);
      for (int i = 0; i < 960 * channels; i++) printf(" %.9g", (double)pcm[i]);
      printf("\n");
      printf("finalrange %u\n", (unsigned)dec.rng);

      float whole[960 * 2];
      int n = opus_decode_float(od, buf, len, whole, 960, 0);
      printf("whole %d", n * channels);
      for (int i = 0; i < n * channels; i++) printf(" %.9g", (double)whole[i]);
      printf("\n");
   }

   opus_decoder_destroy(od);
   free(cd);
   free(sd);
   return 0;
}
