/* Variadic functions: int, long and pointer arguments, and va_copy to walk the
 * same list twice. */
#include <stdarg.h>

static long mix(int n, ...) {
    va_list ap, again;
    va_start(ap, n);
    va_copy(again, ap);
    long s = 0;
    for (int i = 0; i < n; i++)
        s = s * 3 + va_arg(ap, int);
    for (int i = 0; i < n; i++)
        s ^= va_arg(again, int) << i;
    va_end(again);
    va_end(ap);
    return s;
}

static long pick(const char *fmt, ...) {
    va_list ap;
    va_start(ap, fmt);
    long s = 0;
    for (const char *p = fmt; *p; p++) {
        if (*p == 'l') s += va_arg(ap, long);
        else if (*p == 's') s += *va_arg(ap, const char *);
        else s -= va_arg(ap, int);
    }
    va_end(ap);
    return s;
}

int test_main(void) {
    return (int)(mix(5, 1, -2, 3, 40, 5) + mix(0) + pick("lis", 1L << 40 >> 30, 7, "A"));
}
