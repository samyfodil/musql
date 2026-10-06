/* Times the benchmark workloads against the SQLite C API directly, with no Go,
 * cgo or database/sql in the path: the native-C baseline for
 * TestBenchColumnarVsC.
 *
 * Input (argv[2]): lines "W\t<name>\t<sql>" each followed by bind-value lines
 * "A\t<comma-separated integers>". Output: "<name>\t<nanoseconds per query>".
 * The timing loop matches the Go arms: one warm-up, an iteration count sized to
 * ~200 ms from it (floor 40, ceiling 200000, 3 for a statement slower than the
 * target), every row stepped, the statement reset between runs. */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include "sqlite3.h"

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

static void run(sqlite3_stmt *st, const workload *w, int set) {
    sqlite3_reset(st);
    for (int i = 0; i < w->nargs; i++)
        sqlite3_bind_int64(st, i + 1, w->args[(long)set * w->nargs + i]);
    int rc;
    while ((rc = sqlite3_step(st)) == SQLITE_ROW)
        ;
    if (rc != SQLITE_DONE) {
        fprintf(stderr, "%s: %s\n", w->name, sqlite3_errmsg(sqlite3_db_handle(st)));
        exit(1);
    }
}

static int count_args(const char *csv) {
    if (!*csv) return 0;
    int n = 1;
    for (; *csv; csv++) n += *csv == ',';
    return n;
}

int main(int argc, char **argv) {
    if (argc != 3) {
        fprintf(stderr, "usage: cbench <db> <workloads>\n");
        return 2;
    }
    sqlite3 *db;
    if (sqlite3_open_v2(argv[1], &db, SQLITE_OPEN_READONLY, NULL) != SQLITE_OK) {
        fprintf(stderr, "open: %s\n", sqlite3_errmsg(db));
        return 1;
    }
    FILE *f = fopen(argv[2], "r");
    if (!f) { perror(argv[2]); return 1; }

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

    for (int k = 0; k < nw; k++) {
        workload *w = &ws[k];
        sqlite3_stmt *st;
        if (sqlite3_prepare_v2(db, w->sql, -1, &st, NULL) != SQLITE_OK) {
            fprintf(stderr, "%s: %s\n", w->name, sqlite3_errmsg(db));
            return 1;
        }
        long long t0 = now_ns();
        run(st, w, 0);
        long long one = now_ns() - t0;
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
        printf("%s\t%lld\n", w->name, (now_ns() - t0) / n);
        sqlite3_finalize(st);
    }
    sqlite3_close(db);
    return 0;
}
