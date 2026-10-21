/* Times the benchmark workloads against Turso's own C API (the
 * libturso_sync_sdk_kit library its Go driver loads), with no Go in the path,
 * so Turso is measured the way cbench.c measures C SQLite: one prepared
 * statement, rebound, stepped and reset per query. Usage:
 * tbench <db> <workloads>. Declarations follow the library's ABI as the Go
 * driver (turso.tech/database/tursogo, bindings_db.go) declares it. */
#include <stdint.h>
#include "workloads.h"

typedef struct turso_database turso_database_t;
typedef struct turso_connection turso_connection_t;
typedef struct turso_statement turso_statement_t;

typedef struct {
    void *logger;
    const char *log_level;
} turso_config_t;

typedef struct {
    uint64_t async_io;
    const char *path, *experimental_features, *vfs, *encryption_cipher, *encryption_hexkey;
} turso_database_config_t;

enum { TURSO_OK = 0, TURSO_DONE = 1, TURSO_ROW = 2, TURSO_IO = 3 };
enum { TURSO_TYPE_INTEGER = 1, TURSO_TYPE_REAL = 2, TURSO_TYPE_TEXT = 3, TURSO_TYPE_BLOB = 4 };

int32_t turso_setup(const turso_config_t *, const char **err);
int32_t turso_database_new(const turso_database_config_t *, turso_database_t **, const char **err);
int32_t turso_database_open(turso_database_t *, const char **err);
int32_t turso_database_connect(turso_database_t *, turso_connection_t **, const char **err);
int32_t turso_connection_prepare_single(turso_connection_t *, const char *sql, turso_statement_t **, const char **err);
int32_t turso_statement_step(turso_statement_t *, const char **err);
int32_t turso_statement_run_io(turso_statement_t *, const char **err);
int32_t turso_statement_reset(turso_statement_t *, const char **err);
int32_t turso_statement_finalize(turso_statement_t *, const char **err);
int64_t turso_statement_column_count(turso_statement_t *);
int32_t turso_statement_row_value_kind(turso_statement_t *, size_t);
int64_t turso_statement_row_value_bytes_count(turso_statement_t *, size_t);
const void *turso_statement_row_value_bytes_ptr(turso_statement_t *, size_t);
int64_t turso_statement_row_value_int(turso_statement_t *, size_t);
double turso_statement_row_value_double(turso_statement_t *, size_t);
int32_t turso_statement_bind_positional_int(turso_statement_t *, size_t, int64_t);
void turso_statement_deinit(turso_statement_t *);

static void fail(const char *what, const char *err) {
    fprintf(stderr, "%s: %s\n", what, err ? err : "failed");
    exit(1);
}

static void run(void *p, const workload *w, int set) {
    turso_statement_t *st = p;
    const char *err = NULL;
    if (turso_statement_reset(st, &err) != TURSO_OK) fail(w->name, err);
    for (int i = 0; i < w->nargs; i++)
        if (turso_statement_bind_positional_int(st, (size_t)i + 1, w->args[(long)set * w->nargs + i]) != TURSO_OK)
            fail(w->name, "bind");
    size_t ncol = (size_t)turso_statement_column_count(st);
    for (;;) {
        int32_t rc = turso_statement_step(st, &err);
        if (rc == TURSO_DONE) return;
        if (rc == TURSO_IO) {
            if (turso_statement_run_io(st, &err) != TURSO_OK) fail(w->name, err);
            continue;
        }
        if (rc == TURSO_OK) continue;
        if (rc != TURSO_ROW) fail(w->name, err);
        for (size_t c = 0; c < ncol; c++) {
            switch (turso_statement_row_value_kind(st, c)) {
            case TURSO_TYPE_INTEGER: { long long v = turso_statement_row_value_int(st, c); mix(&v, sizeof v); break; }
            case TURSO_TYPE_REAL: { double v = turso_statement_row_value_double(st, c); mix(&v, sizeof v); break; }
            case TURSO_TYPE_TEXT: case TURSO_TYPE_BLOB:
                mix(turso_statement_row_value_bytes_ptr(st, c), (size_t)turso_statement_row_value_bytes_count(st, c));
                break;
            default: mix("", 1);
            }
        }
    }
}

int main(int argc, char **argv) {
    if (argc != 3) {
        fprintf(stderr, "usage: tbench <db> <workloads>\n");
        return 2;
    }
    const char *err = NULL;
    turso_config_t cfg = {0};
    if (turso_setup(&cfg, &err) != TURSO_OK) fail("setup", err);
    turso_database_config_t dc = {0};
    dc.path = argv[1];
    turso_database_t *db;
    turso_connection_t *conn;
    if (turso_database_new(&dc, &db, &err) != TURSO_OK) fail("new", err);
    if (turso_database_open(db, &err) != TURSO_OK) fail("open", err);
    if (turso_database_connect(db, &conn, &err) != TURSO_OK) fail("connect", err);
    int nw;
    workload *ws = load_workloads(argv[2], &nw);
    for (int k = 0; k < nw; k++) {
        turso_statement_t *st;
        if (turso_connection_prepare_single(conn, ws[k].sql, &st, &err) != TURSO_OK) fail(ws[k].name, err);
        time_workload(st, &ws[k], run);
        turso_statement_finalize(st, &err);
        turso_statement_deinit(st);
    }
    return 0;
}
