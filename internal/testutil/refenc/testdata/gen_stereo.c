/* Encodes a raw signal with the RFC 6716 reference encoder, forcing configurations opusenc will not
   produce on its own.

   opusenc chooses its own channel count and mode: below about fourteen kilobits it downmixes to
   mono, and above it moves to hybrid, so a SILK stereo packet cannot be asked for. The reference
   encoder can be told directly.

   Build (from the extracted reference root):
     gcc -O2 -DVAR_ARRAYS -I celt -I silk -I silk/float -I include -I src \
         -o gen gen_stereo.c celt/*.c silk/*.c silk/float/*.c src/*.c -lm

   Run: gen <raw-s16le-48k-stereo> <out.opus-packets> <bitrate> <force-mode>
        force-mode: 1000 SILK only, 1001 hybrid, 1002 CELT only, as opus_private.h numbers them
   Writes a simple length-prefixed packet stream, which the Go side wraps in Ogg.
*/
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include "opus.h"
#include "opus_defines.h"
/* Forcing the mode is an internal control, which is the only way to ask for a configuration the
   encoder would not choose. */
#include "opus_private.h"

int main(int argc, char **argv)
{
   if (argc != 5) {
      fprintf(stderr, "usage: gen <raw> <out> <bitrate> <mode>\n");
      return 2;
   }
   int bitrate = atoi(argv[3]);
   int mode = atoi(argv[4]);

   int err;
   OpusEncoder *enc = opus_encoder_create(48000, 2, OPUS_APPLICATION_VOIP, &err);
   if (err != OPUS_OK) { fprintf(stderr, "create: %s\n", opus_strerror(err)); return 1; }

   opus_encoder_ctl(enc, OPUS_SET_BITRATE(bitrate));
   opus_encoder_ctl(enc, OPUS_SET_FORCE_CHANNELS(2));
   opus_encoder_ctl(enc, OPUS_SET_VBR(0));
   opus_encoder_ctl(enc, OPUS_SET_COMPLEXITY(10));
   if (mode == MODE_SILK_ONLY) {
      /* Wideband is as far as SILK reaches; asking for more would hand over to the other codec. */
      opus_encoder_ctl(enc, OPUS_SET_MAX_BANDWIDTH(OPUS_BANDWIDTH_WIDEBAND));
   } else if (mode == MODE_HYBRID) {
      /* Hybrid needs a width past what SILK covers, or there is nothing for the transform codec
         to add and the encoder has no reason to choose it. */
      opus_encoder_ctl(enc, OPUS_SET_BANDWIDTH(OPUS_BANDWIDTH_SUPERWIDEBAND));
   }
   opus_encoder_ctl(enc, OPUS_SET_FORCE_MODE(mode));

   FILE *in = fopen(argv[1], "rb");
   FILE *out = fopen(argv[2], "wb");
   if (!in || !out) { perror("open"); return 1; }

   const int frame = 960; /* 20 ms at 48 kHz */
   short pcm[960 * 2];
   unsigned char packet[4000];
   int packets = 0;

   while (fread(pcm, sizeof(short), frame * 2, in) == (size_t)(frame * 2)) {
      /* FORCE_MODE is cleared after every call, so it is set again each time. */
      opus_encoder_ctl(enc, OPUS_SET_FORCE_MODE(mode));
      int n = opus_encode(enc, pcm, frame, packet, sizeof packet);
      if (n < 0) { fprintf(stderr, "encode: %s\n", opus_strerror(n)); return 1; }
      unsigned char len[2] = { (unsigned char)(n >> 8), (unsigned char)n };
      fwrite(len, 1, 2, out);
      fwrite(packet, 1, n, out);
      packets++;
   }

   fclose(in);
   fclose(out);
   opus_encoder_destroy(enc);
   fprintf(stderr, "gen_stereo: wrote %d packets\n", packets);
   return 0;
}
