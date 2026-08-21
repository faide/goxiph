/* Oracle for the bit-exact split arithmetic, built from the RFC 6716 reference definitions.
   Prints test vectors consumed by TestBitexactMatchesReference. Run: cc -o gen gen_bitexact.c */
#include <stdio.h>
#include <stdint.h>

#define FRAC_MUL16(a,b) ((16384+((int32_t)(int16_t)(a)*(int16_t)(b)))>>15)
#define BITRES 3
#define IMIN(a,b) ((a)<(b)?(a):(b))

/* celt/bands.c */
static int16_t bitexact_cos(int16_t x)
{
   int32_t tmp;
   int16_t x2;
   tmp = (4096+((int32_t)(x)*(x)))>>13;
   x2 = tmp;
   x2 = (32767-x2) + FRAC_MUL16(x2, (-7651 + FRAC_MUL16(x2, (8277 + FRAC_MUL16(-626, x2)))));
   return 1+x2;
}

static int ec_ilog(uint32_t _v){
  int ret; int m;
  ret=!!_v;
  m=!!(_v&0xFFFF0000)<<4; _v>>=m; ret|=m;
  m=!!(_v&0xFF00)<<3; _v>>=m; ret|=m;
  m=!!(_v&0xF0)<<2; _v>>=m; ret|=m;
  m=!!(_v&0xC)<<1; _v>>=m; ret|=m;
  ret+=!!(_v&0x2);
  return ret;
}
#define EC_ILOG(x) (ec_ilog(x))

static int bitexact_log2tan(int isin,int icos)
{
   int lc, ls;
   lc=EC_ILOG(icos);
   ls=EC_ILOG(isin);
   icos<<=15-lc;
   isin<<=15-ls;
   return (ls-lc)*(1<<11)
         +FRAC_MUL16(isin, FRAC_MUL16(isin, -2597) + 7932)
         -FRAC_MUL16(icos, FRAC_MUL16(icos, -2597) + 7932);
}

/* celt/bands.c */
static int compute_qn(int N, int b, int offset, int pulse_cap, int stereo)
{
   static const int16_t exp2_table8[8] =
      {16384, 17866, 19483, 21247, 23170, 25267, 27554, 30048};
   int qn, qb;
   int N2 = 2*N-1;
   if (stereo && N==2) N2--;
   qb = IMIN(b-pulse_cap-(4<<BITRES), (b+N2*offset)/N2);
   qb = IMIN(8<<BITRES, qb);
   if (qb<(1<<BITRES>>1)) { qn = 1; }
   else { qn = exp2_table8[qb&0x7]>>(14-(qb>>BITRES)); qn = (qn+1)>>1<<1; }
   return qn;
}

int main(void)
{
   int i;
   /* itheta = i*16384/qn with qn <= 256, so every reachable angle is a multiple of 64.
      The endpoints 0 and 16384 are handled by the caller and never reach bitexact_cos:
      at 0 the polynomial returns 32768, which overflows the int16 return. */
   printf("# cos: x -> bitexact_cos(x)\n");
   for (i = 64; i < 16384; i += 64)
      printf("cos %d %d\n", i, bitexact_cos((int16_t)i));

   printf("# log2tan: isin icos -> bitexact_log2tan\n");
   for (i = 64; i < 16384; i += 64) {
      int a = bitexact_cos((int16_t)i);
      int b = bitexact_cos((int16_t)(16384-i));
      printf("log2tan %d %d %d\n", a, b, bitexact_log2tan(a,b));
   }

   printf("# qn: N b offset pulse_cap stereo -> qn\n");
   int Ns[] = {2,3,4,8,16,32,64,100,176};
   int bs[] = {0,8,16,32,64,128,256,512,1024,2048};
   int offs[] = {-16,-4,0,4,16,40};
   int caps[] = {0,8,24,48,96};
   for (int a=0;a<9;a++) for (int c=0;c<10;c++) for (int d=0;d<6;d++)
     for (int e=0;e<5;e++) for (int s=0;s<2;s++)
       printf("qn %d %d %d %d %d %d\n", Ns[a],bs[c],offs[d],caps[e],s,
              compute_qn(Ns[a],bs[c],offs[d],caps[e],s));
   return 0;
}
