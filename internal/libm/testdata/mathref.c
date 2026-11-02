// mathref OUTDIR [N]: writes OUTDIR/<fn>.bin, records of (x, y, libm result) as
// little-endian doubles, from fixed pseudo-random inputs, so a port can be
// compared bit for bit against this machine's libm.
#define _GNU_SOURCE
#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static uint64_t st;
static uint64_t next(void) {
  uint64_t z = (st += 0x9e3779b97f4a7c15ULL);
  z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9ULL;
  z = (z ^ (z >> 27)) * 0x94d049bb133111ebULL;
  return z ^ (z >> 31);
}
static double unif(double a, double b) { return a + (b - a) * ((next() >> 11) * 0x1p-53); }
static double bits(void) {
  for (;;) {
    uint64_t u = next();
    double d;
    memcpy(&d, &u, 8);
    if (isfinite(d)) return d;
  }
}
static double pbits(void) { return fabs(bits()); }
static int pick(void) { return next() % 100; }

static const double specials[] = {0.0, -0.0, 1.0, -1.0, 0.5, -0.5, 2.0, -2.0, 10.0, 100.0, 1e-300, -1e-300,
  0x1p-1074, -0x1p-1074, 0x1p-1022, 0x1.fffffffffffffp+1023, -0x1.fffffffffffffp+1023, INFINITY, -INFINITY,
  3.141592653589793, 1.5707963267948966, 0.7853981633974483, 709.782712893384, -745.1332191019411,
  1e10, 1e22, 1e300, 0.1, 0.25, 0.75, 1.5, 3.0, 0.99, 1.01, 1e-8, 0.001, 30.0, 710.0, -710.0, 22.0, 21.0, 19.0};
#define NSPEC (sizeof specials / sizeof specials[0])

typedef double (*f1)(double);
typedef double (*f2)(double, double);
struct fn { const char *name; f1 one; f2 two; double (*gx)(void); double (*gy)(void); };

static double g_exp(void) { int p = pick(); return p < 40 ? unif(-745.2, 709.8) : p < 70 ? unif(-1, 1) : p < 80 ? unif(-1e-5, 1e-5) : p < 90 ? bits() : unif(-20, 20); }
static double g_log(void) { int p = pick(); return p < 50 ? pbits() : p < 80 ? unif(0.9, 1.1) : p < 90 ? unif(0, 10) : p < 95 ? unif(1 - 1e-6, 1 + 1e-6) : bits(); }
static double g_powx(void) { int p = pick(); return p < 40 ? pbits() : p < 70 ? unif(0, 10) : p < 80 ? bits() : unif(1 - 1e-3, 1 + 1e-3); }
static double g_powy(void) { int p = pick(); return p < 40 ? unif(-20, 20) : p < 60 ? (double)(int)unif(-100, 100) : p < 80 ? bits() : unif(-1100, 1100); }
static double g_trig(void) { int p = pick(); return p < 30 ? unif(-10, 10) : p < 50 ? unif(-1e3, 1e3) : p < 70 ? unif(-1e9, 1e9) : p < 90 ? bits() : unif(-1e-5, 1e-5); }
static double g_unit(void) { int p = pick(); return p < 70 ? unif(-1, 1) : p < 80 ? unif(1 - 1e-6, 1) : p < 90 ? unif(-1, -1 + 1e-6) : bits(); }
static double g_atan(void) { int p = pick(); return p < 40 ? unif(-10, 10) : p < 70 ? bits() : unif(-1e10, 1e10); }
static double g_hyp(void) { int p = pick(); return p < 40 ? unif(-30, 30) : p < 70 ? unif(-720, 720) : p < 90 ? unif(-1, 1) : bits(); }
static double g_tanh(void) { int p = pick(); return p < 40 ? unif(-25, 25) : p < 70 ? unif(-1, 1) : p < 90 ? unif(-1e-8, 1e-8) : bits(); }
static double g_asinh(void) { int p = pick(); return p < 40 ? unif(-10, 10) : p < 70 ? bits() : unif(-1, 1); }
static double g_acosh(void) { int p = pick(); return p < 40 ? unif(1, 10) : p < 70 ? pbits() : unif(1, 1 + 1e-3); }
static double g_any(void) { int p = pick(); return p < 50 ? bits() : unif(-1e6, 1e6); }
static double g_small(void) { int p = pick(); return p < 40 ? unif(-1, 1) : p < 60 ? unif(-1e-10, 1e-10) : p < 80 ? unif(-40, 710) : bits(); }
static double g_log1p(void) { int p = pick(); return p < 40 ? unif(-1, 1) : p < 60 ? unif(-1e-10, 1e-10) : p < 80 ? pbits() : unif(-1, 100); }
static double g_none(void) { return 0; }

static double xfmod(double a, double b) { return fmod(a, b); }
static double deg2rad(double x) { return x * (M_PI / 180.0); }
static double rad2deg(double x) { return x * (180.0 / M_PI); }

int main(int argc, char **argv) {
  if (argc < 2) { fprintf(stderr, "usage: mathref OUTDIR [N]\n"); return 2; }
  long n = argc > 2 ? atol(argv[2]) : 1 << 20;
  struct fn fns[] = {
    {"exp", exp, 0, g_exp, g_none}, {"log", log, 0, g_log, g_none}, {"log2", log2, 0, g_log, g_none},
    {"log10", log10, 0, g_log, g_none}, {"pow", 0, pow, g_powx, g_powy},
    {"sin", sin, 0, g_trig, g_none}, {"cos", cos, 0, g_trig, g_none}, {"tan", tan, 0, g_trig, g_none},
    {"asin", asin, 0, g_unit, g_none}, {"acos", acos, 0, g_unit, g_none}, {"atan", atan, 0, g_atan, g_none},
    {"atan2", 0, atan2, g_atan, g_atan}, {"sinh", sinh, 0, g_hyp, g_none}, {"cosh", cosh, 0, g_hyp, g_none},
    {"tanh", tanh, 0, g_tanh, g_none}, {"asinh", asinh, 0, g_asinh, g_none}, {"acosh", acosh, 0, g_acosh, g_none},
    {"atanh", atanh, 0, g_unit, g_none}, {"expm1", expm1, 0, g_small, g_none}, {"log1p", log1p, 0, g_log1p, g_none},
    {"sqrt", sqrt, 0, g_any, g_none}, {"fmod", 0, xfmod, g_any, g_any}, {"radians", deg2rad, 0, g_any, g_none},
    {"degrees", rad2deg, 0, g_any, g_none},
  };
  char path[4096];
  for (size_t f = 0; f < sizeof fns / sizeof fns[0]; f++) {
    st = 0x5eed0000ULL + f;
    snprintf(path, sizeof path, "%s/%s.bin", argv[1], fns[f].name);
    FILE *o = fopen(path, "wb");
    if (!o) { perror(path); return 1; }
    for (long i = 0; i < n + (long)(NSPEC * NSPEC); i++) {
      double x, y = 0, r;
      if (i < (long)NSPEC * (long)NSPEC) {
        x = specials[i % NSPEC];
        if (fns[f].two) y = specials[i / NSPEC]; else if (i >= (long)NSPEC) continue;
      } else {
        x = fns[f].gx();
        if (fns[f].two) y = fns[f].gy();
      }
      r = fns[f].two ? fns[f].two(x, y) : fns[f].one(x);
      double rec[3] = {x, y, r};
      fwrite(rec, 8, 3, o);
    }
    fclose(o);
  }
  return 0;
}
