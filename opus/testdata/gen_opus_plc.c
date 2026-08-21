/* Oracle for Opus packet loss concealment, built against the RFC 6716 reference.

   Decodes a run of packets out of a test vector with some of them thrown away, and prints what the
   reference plays. The run matters: concealment extrapolates from the last good packet and the
   packet after a loss decodes from the state concealment left, so a single loss in isolation
   exercises only half of it.

   Build (from the extracted reference root):
     gcc -O2 -DVAR_ARRAYS -DOPUS_BUILD -w -I celt -I silk -I silk/float -I include -I src \
         -o gen gen_opus_plc.c $(ls src/*.c|grep -vE 'demo|compare') silk/*.c silk/float/*.c \
         $(ls celt/*.c|grep -v demo) -lm

   Run: gen <bitfile> <first-packet> <count> <loss-mask>
   The mask is one character per packet, '1' for lost. A lost packet is concealed for as long as its
   own payload says, which is what a container knows and the codec does not.
*/
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "opus.h"

int main(int argc, char **argv)
{
   if (argc != 5) { fprintf(stderr, "usage: gen <bitfile> <first> <count> <mask>\n"); return 2; }
   const char *path = argv[1];
   int first = atoi(argv[2]), count = atoi(argv[3]);
   const char *mask = argv[4];

   FILE *f = fopen(path, "rb");
   if (!f) { perror("open"); return 1; }

   int err;
   OpusDecoder *dec = opus_decoder_create(48000, 2, &err);
   if (!dec) { fprintf(stderr, "decoder: %s\n", opus_strerror(err)); return 1; }

   printf("case %s first %d count %d losses %s\n", path, first, count, mask);

   static unsigned char buf[4096];
   static opus_int16 pcm[5760 * 2];
   int index = 0, played = 0, prev_lost = 0;
   unsigned char hdr[8];
   while (fread(hdr, 1, 8, f) == 8) {
      int len = (hdr[0] << 24) | (hdr[1] << 16) | (hdr[2] << 8) | hdr[3];
      if (len <= 0 || len > (int)sizeof buf) break;
      if ((int)fread(buf, 1, len, f) != len) break;
      if (index++ < first) continue;
      if (played >= count) break;

      int lost = played < (int)strlen(mask) && mask[played] == '1';
      int samples = opus_decoder_get_nb_samples(dec, buf, len);

      int n;
      if (lost) {
         printf("lost %d %d\n", played, samples);
         n = opus_decode(dec, NULL, 0, pcm, samples, 0);
      } else {
         printf("packethex %d ", played);
         for (int k = 0; k < len; k++) printf("%02x", buf[k]);
         printf("\n");
         n = opus_decode(dec, buf, len, pcm, 5760, 0);
      }
      if (n < 0) { fprintf(stderr, "decode: %s\n", opus_strerror(n)); return 1; }

      /* Only the frames concealment touches are printed. The rest are the ordinary decode the
         other vectors already cover. */
      if (lost || prev_lost) {
         printf("out %d %d", played, n * 2);
         for (int k = 0; k < n * 2; k++) printf(" %d", (int)pcm[k]);
         printf("\n");
      }
      prev_lost = lost;
      played++;
   }

   opus_decoder_destroy(dec);
   fclose(f);
   return 0;
}
