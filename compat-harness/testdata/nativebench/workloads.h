/* The workload file and timing loop both native harnesses share, so the C
 * SQLite and Turso arms differ only in the library each one calls.
 *
 * Input: lines "W\t<name>\t<sql>" each followed by bind-value lines
 * "A\t<comma-separated integers>". Output per workload:
 * "<name>\t<nanoseconds per query>\t<checksum>". The timing loop matches the
 * Go arms: one warm-up, an iteration count sized to ~200 ms from it (floor 40,
 * ceiling 200000, 3 for a statement slower than the target). Every row is
 * stepped and every column of it read, as a Go arm materializes each row; the
 * checksum, over the first bind set's rows, lets the caller check that both
 * libraries read the same values. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#define TARGET_NS 200000000LL

typedef struct {
    char name[256];
    char *sql;
    long long *args; /* nsets * nargs */
    int nargs, nsets, cap;
} workload;

static long long now_ns(void) {
    struct timespec ts;
    clock_gettime(CLOCK_MONOTONIC, &ts);
    return (long long)ts.tv_sec * 1000000000LL + ts.tv_nsec;
}

/* FNV-1a over the values read, so the compiler cannot drop the reads. */
static unsigned long long sum;

static void mix(const void *p, size_t n) {
    const unsigned char *b = p;
    for (size_t i = 0; i < n; i++) sum = (sum ^ b[i]) * 1099511628211ULL;
}

static int count_args(const char *csv) {
    if (!*csv) return 0;
    int n = 1;
    for (; *csv; csv++) n += *csv == ',';
    return n;
}

static workload *load_workloads(const char *path, int *count) {
    FILE *f = fopen(path, "r");
    if (!f) { perror(path); exit(1); }
    static char line[1 << 16];
    workload *ws = NULL;
    int nw = 0;
    while (fgets(line, sizeof line, f)) {
        line[strcspn(line, "\n")] = 0;
        if (line[0] == 'W') {
            ws = realloc(ws, sizeof *ws * (nw + 1));
            workload *w = &ws[nw++];
            memset(w, 0, sizeof *w);
            char *name = line + 2, *tab = strchr(name, '\t');
            *tab = 0;
            snprintf(w->name, sizeof w->name, "%s", name);
            w->sql = strdup(tab + 1);
        } else if (line[0] == 'A') {
            workload *w = &ws[nw - 1];
            char *csv = line + 2;
            if (w->nsets == 0) w->nargs = count_args(csv);
            if (w->nsets == w->cap) {
                w->cap = w->cap ? w->cap * 2 : 64;
                w->args = realloc(w->args, sizeof(long long) * w->cap * (w->nargs ? w->nargs : 1));
            }
            char *p = csv;
            for (int i = 0; i < w->nargs; i++) {
                w->args[(long)w->nsets * w->nargs + i] = strtoll(p, &p, 10);
                if (*p == ',') p++;
            }
            w->nsets++;
        }
    }
    fclose(f);
    *count = nw;
    return ws;
}

/* time_workload runs one prepared statement through run(st, w, set) and prints
 * its line. */
static void time_workload(void *st, const workload *w, void (*run)(void *, const workload *, int)) {
    sum = 14695981039346656037ULL;
    long long t0 = now_ns();
    run(st, w, 0);
    long long one = now_ns() - t0;
    unsigned long long check = sum;
    long long n;
    if (one >= TARGET_NS) n = 3;
    else {
        n = one > 0 ? TARGET_NS / one : 200000;
        if (n > 200000) n = 200000;
        if (n < 40) n = 40;
    }
    t0 = now_ns();
    for (long long i = 0; i < n; i++)
        run(st, w, (int)(i % w->nsets));
    printf("%s\t%lld\t%llu\n", w->name, (now_ns() - t0) / n, check);
}
