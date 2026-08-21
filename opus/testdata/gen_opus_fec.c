/* Oracle for Opus forward error correction, built against the RFC 6716 reference.

   Encodes a stream that carries in-band redundancy, then decodes it with packets thrown away and
   recovered from the redundant copy the following packet carries. Both halves are here because
   nothing else produces such a stream: an encoder only emits the redundancy when it is told to
   expect loss, and no tool in the corpus offers that.

   The reference to build against is a corrected copy: the source frozen into RFC 6716 predates
   RFC 8251, and hybrid frames differ without its folding change. `mise run specs:corrected` makes
   one from the extracted tree.

   Build (from the corrected reference root):
     gcc -O2 -DVAR_ARRAYS -DOPUS_BUILD -w -I celt -I silk -I silk/float -I include -I src \
         -o gen gen_opus_fec.c $(ls src/*.c|grep -vE 'demo|compare') silk/*.c silk/float/*.c \
         $(ls celt/*.c|grep -v demo) -lm

   Run: gen <raw-s16le-48k-mono> <bitrate> <frame-ms> <packets> <loss-mask>
*/
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "opus.h"

#define MAXP 64

int main(int argc, char **argv)
{
   if (argc != 6) { fprintf(stderr, "usage: gen <pcm> <bitrate> <ms> <packets> <mask>\n"); return 2; }
   const char *path = argv[1];
   int bitrate = atoi(argv[2]), ms = atoi(argv[3]), want = atoi(argv[4]);
   const char *mask = argv[5];
   int frame = 48 * ms;

   FILE *f = fopen(path, "rb");
   if (!f) { perror("open"); return 1; }

   int err;
   OpusEncoder *enc = opus_encoder_create(48000, 1, OPUS_APPLICATION_VOIP, &err);
   if (!enc) { fprintf(stderr, "encoder: %s\n", opus_strerror(err)); return 1; }
   opus_encoder_ctl(enc, OPUS_SET_BITRATE(bitrate));
   opus_encoder_ctl(enc, OPUS_SET_INBAND_FEC(1));
   /* The redundancy is only emitted where the encoder is told to expect loss. */
   opus_encoder_ctl(enc, OPUS_SET_PACKET_LOSS_PERC(30));
   opus_encoder_ctl(enc, OPUS_SET_COMPLEXITY(10));

   static unsigned char data[MAXP][1500];
   static int len[MAXP];
   static opus_int16 in[5760];
   int count = 0;
   while (count < want && count < MAXP) {
      if ((int)fread(in, sizeof(opus_int16), frame, f) != frame) break;
      len[count] = opus_encode(enc, in, frame, data[count], sizeof data[0]);
      if (len[count] < 0) { fprintf(stderr, "encode: %s\n", opus_strerror(len[count])); return 1; }
      count++;
   }
   fclose(f);
   opus_encoder_destroy(enc);

   printf("case bitrate %d ms %d packets %d losses %s\n", bitrate, ms, count, mask);
   for (int i = 0; i < count; i++) {
      printf("packethex %d ", i);
      for (int k = 0; k < len[i]; k++) printf("%02x", data[i][k]);
      printf("\n");
   }

   OpusDecoder *dec = opus_decoder_create(48000, 1, &err);
   static opus_int16 pcm[5760];
   int prev_lost = 0;
   for (int i = 0; i < count; i++) {
      int lost = i < (int)strlen(mask) && mask[i] == '1';
      int n;
      if (lost) {
         int samples = opus_decoder_get_nb_samples(dec, data[i], len[i]);
         int next = i + 1 < count && !(i + 1 < (int)strlen(mask) && mask[i + 1] == '1');
         if (next) {
            printf("recover %d %d\n", i, samples);
            n = opus_decode(dec, data[i + 1], len[i + 1], pcm, samples, 1);
         } else {
            printf("lost %d %d\n", i, samples);
            n = opus_decode(dec, NULL, 0, pcm, samples, 0);
         }
      } else {
         n = opus_decode(dec, data[i], len[i], pcm, 5760, 0);
      }
      if (n < 0) { fprintf(stderr, "decode: %s\n", opus_strerror(n)); return 1; }

      if (lost || prev_lost) {
         printf("out %d %d", i, n);
         for (int k = 0; k < n; k++) printf(" %d", (int)pcm[k]);
         printf("\n");
      }
      prev_lost = lost;
   }
   opus_decoder_destroy(dec);
   return 0;
}
