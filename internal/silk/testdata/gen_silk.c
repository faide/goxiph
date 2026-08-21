/* Oracle for the SILK decoder, built against the RFC 6716 reference.

   Reads one SILK payload and prints what the reference decodes from it at every stage: the header
   flags, the per-frame indices, the excitation, and the parameters those indices expand into. That
   makes a divergence in our decoder land on the stage where it happens rather than on the output.

   Build (from the extracted reference root):
     gcc -O2 -DVAR_ARRAYS -I silk -I silk/float -I celt -I include \
         -o gen gen_silk.c silk/*.c silk/float/*.c celt/entdec.c celt/entcode.c -lm

   Run: gen <payload-file> <fs_kHz> <payload_ms> <channels>
*/
#include <stdio.h>
#include <stdlib.h>
#include "main.h"
#include "stack_alloc.h"
#include "entdec.h"
#include <string.h>

static void dumpi(const char *name, const opus_int *v, int n) {
   printf("%s", name);
   for (int i = 0; i < n; i++) printf(" %d", (int)v[i]);
   printf("\n");
}

int main(int argc, char **argv)
{
   if (argc != 5) { fprintf(stderr, "usage: gen <file> <fs_kHz> <ms> <channels>\n"); return 2; }

   FILE *f = fopen(argv[1], "rb");
   if (!f) { perror("open"); return 1; }
   static unsigned char buf[8192];
   int len = (int)fread(buf, 1, sizeof buf, f);
   fclose(f);

   int fs_kHz = atoi(argv[2]), ms = atoi(argv[3]), channels = atoi(argv[4]);

   silk_decoder_state st[2];
   for (int n = 0; n < channels; n++) {
      silk_init_decoder(&st[n]);
      st[n].nFramesPerPacket = (ms <= 20) ? 1 : ms / 20;
      st[n].nb_subfr = (ms == 10) ? 2 : 4;
      silk_decoder_set_fs(&st[n], fs_kHz, 48000);
   }

   ec_dec dec;
   ec_dec_init(&dec, buf, len);

   printf("payload %d bytes fs %d ms %d channels %d\n", len, fs_kHz, ms, channels);

   /* Header: voice-activity flags then the redundancy flag, per channel. */
   for (int n = 0; n < channels; n++) {
      for (int i = 0; i < st[n].nFramesPerPacket; i++)
         st[n].VAD_flags[i] = ec_dec_bit_logp(&dec, 1);
      st[n].LBRR_flag = ec_dec_bit_logp(&dec, 1);
      printf("vad %d", n);
      for (int i = 0; i < st[n].nFramesPerPacket; i++) printf(" %d", (int)st[n].VAD_flags[i]);
      printf(" lbrr %d\n", (int)st[n].LBRR_flag);
   }
   for (int n = 0; n < channels; n++) {
      for (int i = 0; i < MAX_FRAMES_PER_PACKET; i++) st[n].LBRR_flags[i] = 0;
      if (st[n].LBRR_flag) {
         if (st[n].nFramesPerPacket == 1) st[n].LBRR_flags[0] = 1;
         else {
            int sym = ec_dec_icdf(&dec, silk_LBRR_flags_iCDF_ptr[st[n].nFramesPerPacket - 2], 8) + 1;
            for (int i = 0; i < st[n].nFramesPerPacket; i++)
               st[n].LBRR_flags[i] = (sym >> i) & 1;
         }
      }
      printf("lbrrflags %d", n);
      for (int i = 0; i < st[n].nFramesPerPacket; i++) printf(" %d", (int)st[n].LBRR_flags[i]);
      printf("\n");
   }

   /* Redundant frames are decoded and thrown away, but they still consume symbols. */
   opus_int32 MS_pred_Q13[2] = {0, 0};
   for (int i = 0; i < st[0].nFramesPerPacket; i++) {
      for (int n = 0; n < channels; n++) {
         if (!st[n].LBRR_flags[i]) continue;
         opus_int pulses[MAX_FRAME_LENGTH];
         if (channels == 2 && n == 0) {
            silk_stereo_decode_pred(&dec, MS_pred_Q13);
            if (st[1].LBRR_flags[i] == 0) {
               opus_int mid_only;
               silk_stereo_decode_mid_only(&dec, &mid_only);
            }
         }
         int cond = (i > 0 && st[n].LBRR_flags[i-1]) ? CODE_CONDITIONALLY : CODE_INDEPENDENTLY;
         silk_decode_indices(&st[n], &dec, i, 1, cond);
         silk_decode_pulses(&dec, pulses, st[n].indices.signalType,
                            st[n].indices.quantOffsetType, st[n].frame_length);
         printf("lbrrskip %d %d\n", i, n);
      }
   }

   /* The frames that are actually played. */
   for (int i = 0; i < st[0].nFramesPerPacket; i++) {
      int decode_only_middle = 0;
      for (int n = 0; n < channels; n++) {
         if (channels == 2 && n == 0) {
            silk_stereo_decode_pred(&dec, MS_pred_Q13);
            printf("stereopred %d %d %d\n", i, (int)MS_pred_Q13[0], (int)MS_pred_Q13[1]);
            if (st[1].VAD_flags[i] == 0) {
               silk_stereo_decode_mid_only(&dec, &decode_only_middle);
               printf("midonly %d %d\n", i, decode_only_middle);
            }
         }
         if (n == 1 && decode_only_middle) continue;

         int cond = (i > 0 && !st[n].LBRR_flags[i-1]) ? CODE_CONDITIONALLY : CODE_INDEPENDENTLY;
         if (i == 0) cond = CODE_INDEPENDENTLY;

         silk_decode_indices(&st[n], &dec, i, 0, cond);
         SideInfoIndices *ix = &st[n].indices;
         printf("frame %d %d type %d %d interp %d seed %d\n", i, n,
                (int)ix->signalType, (int)ix->quantOffsetType,
                (int)ix->NLSFInterpCoef_Q2, (int)ix->Seed);
         printf("gainidx %d %d %d %d %d %d\n", i, n,
                (int)ix->GainsIndices[0], (int)ix->GainsIndices[1],
                (int)ix->GainsIndices[2], (int)ix->GainsIndices[3]);
         printf("nlsfidx %d %d", i, n);
         for (int k = 0; k <= st[n].LPC_order; k++) printf(" %d", (int)ix->NLSFIndices[k]);
         printf("\n");
         if (ix->signalType == TYPE_VOICED) {
            printf("pitch %d %d lag %d contour %d per %d ltpscale %d ltp %d %d %d %d\n", i, n,
                   (int)ix->lagIndex, (int)ix->contourIndex, (int)ix->PERIndex,
                   (int)ix->LTP_scaleIndex,
                   (int)ix->LTPIndex[0], (int)ix->LTPIndex[1],
                   (int)ix->LTPIndex[2], (int)ix->LTPIndex[3]);
         }

         opus_int pulses[MAX_FRAME_LENGTH];
         silk_decode_pulses(&dec, pulses, ix->signalType, ix->quantOffsetType, st[n].frame_length);
         printf("pulses %d %d %d", i, n, (int)st[n].frame_length);
         for (int k = 0; k < st[n].frame_length; k++) printf(" %d", (int)pulses[k]);
         printf("\n");

         st[n].nFramesDecoded = i;
         silk_decoder_control ctrl;
         ctrl.LTP_scale_Q14 = 0;
         silk_decode_parameters(&st[n], &ctrl, cond);
         printf("gainsq16 %d %d %d %d %d %d\n", i, n,
                (int)ctrl.Gains_Q16[0], (int)ctrl.Gains_Q16[1],
                (int)ctrl.Gains_Q16[2], (int)ctrl.Gains_Q16[3]);
         printf("lpc %d %d %d", i, n, (int)st[n].LPC_order);
         for (int k = 0; k < st[n].LPC_order; k++) printf(" %d", (int)ctrl.PredCoef_Q12[1][k]);
         printf("\n");
         printf("lpc0 %d %d", i, n);
         for (int k = 0; k < st[n].LPC_order; k++) printf(" %d", (int)ctrl.PredCoef_Q12[0][k]);
         printf("\n");
         if (ix->signalType == TYPE_VOICED) {
            printf("pitchl %d %d %d %d %d %d\n", i, n,
                   (int)ctrl.pitchL[0], (int)ctrl.pitchL[1],
                   (int)ctrl.pitchL[2], (int)ctrl.pitchL[3]);
            printf("ltpcoef %d %d", i, n);
            for (int k = 0; k < LTP_ORDER * st[n].nb_subfr; k++)
               printf(" %d", (int)ctrl.LTPCoef_Q14[k]);
            printf("\n");
            printf("ltpscaleq14 %d %d %d\n", i, n, (int)ctrl.LTP_scale_Q14);
         }

         /* Synthesis, then the output-buffer update silk_decode_frame does around it. Both are
            needed: the long-term filter reaches back into that buffer next frame. */
         opus_int16 xq[MAX_FRAME_LENGTH];
         silk_decode_core(&st[n], &ctrl, xq, pulses);
         printf("out %d %d %d", i, n, (int)st[n].frame_length);
         for (int k = 0; k < st[n].frame_length; k++) printf(" %d", (int)xq[k]);
         printf("\n");

         /* Resampled to the rate Opus delivers, which is the last thing SILK does. */
         opus_int16 rs[MAX_FRAME_LENGTH * 6];
         silk_resampler(&st[n].resampler_state, rs, xq, st[n].frame_length);
         int rsLen = st[n].frame_length * 48 / fs_kHz;
         printf("resampled %d %d %d", i, n, rsLen);
         for (int k = 0; k < rsLen; k++) printf(" %d", (int)rs[k]);
         printf("\n");

         int mv_len = st[n].ltp_mem_length - st[n].frame_length;
         memmove(st[n].outBuf, &st[n].outBuf[st[n].frame_length], mv_len * sizeof(opus_int16));
         memcpy(&st[n].outBuf[mv_len], xq, st[n].frame_length * sizeof(opus_int16));
         st[n].lagPrev = ctrl.pitchL[st[n].nb_subfr - 1];

         /* silk_decode_frame clears this once a frame has decoded, and the interpolation of the
            next frame's first half depends on it. Without this the dump shows every frame
            declining to interpolate. */
         st[n].first_frame_after_reset = 0;
         st[n].prevSignalType = ix->signalType;
      }
   }
   printf("finalrange %u\n", (unsigned)dec.rng);
   return 0;
}
