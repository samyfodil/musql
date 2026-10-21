/* Times the benchmark workloads against the SQLite C API directly, with no Go,
 * cgo or database/sql in the path: the native-C baseline for
 * TestBenchColumnarVsC. Usage: cbench <db> <workloads>. */
#include "workloads.h"
#include "sqlite3-binding.h" /* mattn's copy of sqlite3.h, 3.53.3 */

static void run(void *p, const workload *w, int set) {
    sqlite3_stmt *st = p;
    sqlite3_reset(st);
    for (int i = 0; i < w->nargs; i++)
        sqlite3_bind_int64(st, i + 1, w->args[(long)set * w->nargs + i]);
    int rc, ncol = sqlite3_column_count(st);
    while ((rc = sqlite3_step(st)) == SQLITE_ROW) {
        for (int c = 0; c < ncol; c++) {
            switch (sqlite3_column_type(st, c)) {
            case SQLITE_INTEGER: { long long v = sqlite3_column_int64(st, c); mix(&v, sizeof v); break; }
            case SQLITE_FLOAT: { double v = sqlite3_column_double(st, c); mix(&v, sizeof v); break; }
            case SQLITE_TEXT: case SQLITE_BLOB: {
                const void *b = sqlite3_column_blob(st, c);
                mix(b, (size_t)sqlite3_column_bytes(st, c));
                break;
            }
            default: mix("", 1);
            }
        }
    }
    if (rc != SQLITE_DONE) {
        fprintf(stderr, "%s: %s\n", w->name, sqlite3_errmsg(sqlite3_db_handle(st)));
        exit(1);
    }
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
    int nw;
    workload *ws = load_workloads(argv[2], &nw);
    for (int k = 0; k < nw; k++) {
        sqlite3_stmt *st;
        if (sqlite3_prepare_v2(db, ws[k].sql, -1, &st, NULL) != SQLITE_OK) {
            fprintf(stderr, "%s: %s\n", ws[k].name, sqlite3_errmsg(db));
            return 1;
        }
        time_workload(st, &ws[k], run);
        sqlite3_finalize(st);
    }
    sqlite3_close(db);
    return 0;
}
