/* Floating point: float and double arithmetic, conversions both ways, and a
 * small escape-time fractal as an iterative workload. */

volatile double scale = 0.0625;
volatile float bias = 1.5f;

static int escape(double cr, double ci) {
    double zr = 0, zi = 0;
    int n = 0;
    while (n < 60 && zr * zr + zi * zi <= 4.0) {
        double t = zr * zr - zi * zi + cr;
        zi = 2 * zr * zi + ci;
        zr = t;
        n++;
    }
    return n;
}

int test_main(void) {
    int total = 0;
    for (int y = -16; y < 16; y++)
        for (int x = -40; x < 16; x++)
            total += escape(x * scale, y * scale * 2);
    float f = bias;
    for (int i = 0; i < 10; i++)
        f = f * 1.25f - 0.5f;
    double d = -7.9;
    total += (int)d + (int)(f * 100) + (int)(unsigned)(d * -3);
    total += (int)((double)total / 3.0 > 1000.0);
    return total;
}
