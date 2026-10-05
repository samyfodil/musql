/* Hard shapes for the compiler: values live across a recursive
 * call (the reentrancy spill), narrow unsigned div/rem/shift, and a phi swap
 * cycle. Inputs come from volatile globals so nothing constant-folds. */

volatile int vn = 12;
volatile unsigned char vb = 200;
volatile unsigned short vs = 60000;
volatile int vneg = -7;

__attribute__((noinline)) static int ack(int depth, int x) {
    if (depth == 0) return x;
    int a = x * 3 + depth;          /* live across both calls */
    int b = ack(depth - 1, a) ^ a;
    int c = ack(depth - 1, b + 1);
    return (a - b) ^ (c * 7 + 1);
}

__attribute__((noinline)) static unsigned narrow(unsigned char b, unsigned short s, int neg) {
    unsigned char q = b / 7, r = b % 7;
    unsigned short sq = s / 300, sr = s % 300;
    unsigned u = (unsigned)neg >> 3;
    unsigned char sh = (unsigned char)neg >> 2;
    return q + r * 10u + sq * 100u + sr + u % 1000u + sh * 7u;
}

__attribute__((noinline)) static long swaps(int n) {
    long a = 1, b = 2, c = 3;
    for (int i = 0; i < n; i++) { long t = a; a = b; b = t; c += a * i; }
    return a * 10000 + b * 100 + c;
}

int test_main(void) {
    int n = vn;
    return ack(6, n) + (int)narrow(vb, vs, vneg) + (int)swaps(n);
}
