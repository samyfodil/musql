/* Control flow: dense and sparse switches, nested loops with early exits,
 * recursion, mutual recursion, and calls through a table of function
 * pointers. */

volatile int seed = 17;

static int classify(int x) {
    switch (x % 9) {
    case 0: return 5;
    case 1: case 2: return x / 3;
    case 4: return -x;
    case 7: return x * x;
    default: break;
    }
    switch (x) {
    case 100: return 1;
    case 5000: return 2;
    case -40: return 3;
    }
    return 0;
}

static int is_even(unsigned n);
static int is_odd(unsigned n) { return n == 0 ? 0 : is_even(n - 1); }
static int is_even(unsigned n) { return n == 0 ? 1 : is_odd(n - 1); }

static long fact(int n) { return n <= 1 ? 1 : n * fact(n - 1); }
static int hops(long n) { int k = 0; while (n != 1) { n = n % 2 ? 3 * n + 1 : n / 2; k++; } return k; }

static int twice(int x) { return 2 * x; }
static int square(int x) { return x * x; }
static int negate(int x) { return -x; }
static int (*const ops[])(int) = { twice, square, negate };

int test_main(void) {
    int s = 0;
    for (int i = -50; i < 200; i += 3)
        s += classify(i);
    for (int i = 0; i < 10; i++) {
        for (int j = 0; j < 10; j++) {
            if (j > i) break;
            if ((i + j) % 4 == 0) continue;
            s += i * j;
        }
    }
    s += is_even(seed) * 1000 + is_odd(seed + 4) * 100;
    s += (int)(fact(12) % 100003);
    s += hops(seed * 1000 + 1);
    for (int i = 0; i < 9; i++)
        s += ops[i % 3](i + seed);
    return s;
}
