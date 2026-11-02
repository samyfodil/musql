// mathwc FN OUT < INPUTS: records (x, y, libm result) for each input line, a
// hex float or, for two-argument functions, "x,y"; lines that do not parse
// (comments, signaling NaNs) are skipped.
#define _GNU_SOURCE
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

int main(int argc, char **argv) {
  struct { const char *name; double (*one)(double); double (*two)(double, double); } fns[] = {
    {"sin", sin, 0}, {"cos", cos, 0}, {"tan", tan, 0}, {"asin", asin, 0}, {"acos", acos, 0}, {"atan", atan, 0},
    {"atan2", 0, atan2}, {"asinh", asinh, 0}, {"acosh", acosh, 0}, {"atanh", atanh, 0},
    {"exp", exp, 0}, {"log", log, 0}, {"log2", log2, 0}, {"log10", log10, 0}, {"pow", 0, pow},
    {"expm1", expm1, 0}, {"log1p", log1p, 0}, {"sinh", sinh, 0}, {"cosh", cosh, 0}, {"tanh", tanh, 0},
  };
  int f = -1;
  for (size_t i = 0; i < sizeof fns / sizeof fns[0]; i++) if (!strcmp(argv[1], fns[i].name)) f = i;
  if (f < 0) { fprintf(stderr, "unknown function %s\n", argv[1]); return 2; }
  FILE *o = fopen(argv[2], "wb");
  if (!o) { perror(argv[2]); return 1; }
  char line[512];
  long n = 0;
  while (fgets(line, sizeof line, stdin)) {
    if (line[0] == '#' || strstr(line, "snan")) continue;
    char *end;
    double x = strtod(line, &end), y = 0;
    if (end == line) continue;
    if (fns[f].two) {
      if (*end != ',') continue;
      char *p = end + 1;
      y = strtod(p, &end);
      if (end == p) continue;
    }
    double r = fns[f].two ? fns[f].two(x, y) : fns[f].one(x);
    double rec[3] = {x, y, r};
    fwrite(rec, 8, 3, o);
    n++;
  }
  fclose(o);
  fprintf(stderr, "%s: %ld records\n", argv[1], n);
  return 0;
}
