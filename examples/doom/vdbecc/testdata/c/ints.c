/* Integer semantics at every width: wrapping, signedness, shifts, rotates and
 * the min/max/abs/saturation idioms clang turns into intrinsics. */
typedef signed char int8_t;
typedef unsigned char uint8_t;
typedef short int16_t;
typedef int int32_t;
typedef unsigned uint32_t;
typedef long long int64_t;
typedef unsigned long long uint64_t;

volatile int32_t vi = -123456789;
volatile uint32_t vu = 0xdeadbeefu;
volatile int16_t vh = -3000;
volatile uint8_t vc = 250;
volatile int64_t vl = 0x123456789abcdefLL;

static uint32_t rotl(uint32_t x, int k) { return (x << k) | (x >> (32 - k)); }

int test_main(void) {
    int32_t i = vi;
    uint32_t u = vu;
    int16_t h = vh;
    uint8_t c = vc;
    int64_t l = vl;
    uint32_t acc = 0;
    acc += (uint32_t)(i * 31 + 7);            /* signed wrap */
    acc ^= u / 13 + u % 13;                   /* unsigned div/rem */
    acc += (uint32_t)(i / 7) + (uint32_t)(i % 7);
    acc ^= (uint32_t)(int16_t)(h * h);        /* 16-bit truncation */
    acc += (uint8_t)(c + 10);                 /* 8-bit wrap */
    acc ^= rotl(u, 5) + rotl(u, 27);
    acc += u >> 7;
    acc ^= (uint32_t)(i >> 3);
    acc += (uint32_t)(l >> 17) ^ (uint32_t)((uint64_t)l >> 40);
    acc += (uint32_t)(l * 3);
    acc ^= (uint32_t)(i < 0 ? -i : i);
    acc += u > (uint32_t)i ? 1 : 2;           /* unsigned compare */
    acc += c > 200 ? c - 200 : 0;             /* saturating subtract */
    acc ^= (uint32_t)(h < -5 ? h : -5);
    acc += (uint32_t)(u < 1000u ? u : 1000u);
    for (int k = 0; k < 40; k++)
        acc = acc * 1103515245u + 12345u;
    return (int)(acc & 0x7fffffff);
}
