/* Oracle for SILK packet loss concealment, built against the RFC 6716 reference.

   Runs the whole decoder API over a sequence of payloads with some of them thrown away, and prints
   what the reference plays for each. The sequence matters: concealment extrapolates from the last
   good frame and fades as the loss runs on, so a single lost packet in isolation exercises almost
   none of it.

   Build (from the extracted reference root):
     gcc -O2 -DVAR_ARRAYS -I silk -I silk/float -I celt -I include \
         -o gen gen_silk_plc.c silk/*.c silk/float/*.c celt/entdec.c celt/entcode.c -lm

   Run: gen <fs_kHz> <payload_ms> <channels> <loss-mask> <payload-file>...
   The loss mask is one character per payload, '1' for lost.
*/
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "main.h"
#include "stack_alloc.h"
#include "entdec.h"
#include "API.h"

int main(int argc, char **argv)
{
   if (argc < 6) { fprintf(stderr, "usage: gen <fs_kHz> <ms> <channels> <mask> <payload>...\n"); return 2; }

   int fs_kHz = atoi(argv[1]), ms = atoi(argv[2]), channels = atoi(argv[3]);
   const char *mask = argv[4];
   int count = argc - 5;

   opus_int32 size;
   silk_Get_Decoder_Size(&size);
   void *dec = malloc(size);
   silk_InitDecoder(dec);

   silk_DecControlStruct ctl;
   memset(&ctl, 0, sizeof ctl);
   ctl.nChannelsAPI = channels;
   ctl.nChannelsInternal = channels;
   ctl.API_sampleRate = 48000;
   ctl.internalSampleRate = fs_kHz * 1000;
   ctl.payloadSize_ms = ms;

   printf("case fs %d ms %d channels %d losses %s\n", fs_kHz, ms, channels, mask);

   int prev_lost = 0;
   for (int i = 0; i < count; i++) {
      int lost = i < (int)strlen(mask) && mask[i] == '1';

      static unsigned char buf[8192];
      int len = 0;
      if (!lost) {
         FILE *f = fopen(argv[5 + i], "rb");
         if (!f) { perror("open"); return 1; }
         len = (int)fread(buf, 1, sizeof buf, f);
         fclose(f);
         printf("payloadhex ");
         for (int k = 0; k < len; k++) printf("%02x", buf[k]);
         printf("\n");
      } else {
         printf("lost %d\n", i);
      }

      ec_dec rd;
      if (!lost) ec_dec_init(&rd, buf, len);

      /* The API delivers one internal frame per call, so a payload longer than 20 ms takes
         several. The first call of a payload is the one that reads its header. */
      static opus_int16 out[MAX_FRAME_LENGTH * 6 * 2 * 3];
      opus_int32 total = 0;
      int calls = ms <= 20 ? 1 : ms / 20;
      for (int k = 0; k < calls; k++) {
         opus_int32 n = 0;
         silk_Decode(dec, &ctl, lost, k == 0, &rd, out + total * channels, &n);
         total += n;
      }

      /* Only the frames concealment touches are printed: a lost one, and the first good one after
         it, which is where the fade back in happens. The rest are the ordinary decode the other
         vectors already cover, and printing them would multiply the file for nothing. */
      if (lost || prev_lost) {
         printf("out %d %d", i, (int)(total * channels));
         for (int k = 0; k < total * channels; k++) printf(" %d", (int)out[k]);
         printf("\n");
      }
      prev_lost = lost;
   }

   free(dec);
   return 0;
}
