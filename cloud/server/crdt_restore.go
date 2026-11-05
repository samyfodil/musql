package server

import (
	"bytes"
	"context"
	"database/sql"
	sqldriver "database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/engine"
	"github.com/samyfodil/musql/replication"

	"github.com/samyfodil/musql/cloud/state"
)

// restoreTries is how often an in-place restore of a cluster database runs its
// diff again when a commit (a peer's op, a client's write) lands meanwhile.
const restoreTries = 10

// restoreReplicatedInPlace makes a cluster database d's tables what this
// node held at `at`: the difference is written through its replicated handle
// in one transaction, so it replicates like any write (HISTORY.md, "Restore
// in place").
func (s *Server) restoreReplicatedInPlace(ctx context.Context, d *state.Database, at time.Time) error {
	db := s.replHandle(d.ID)
	if db == nil {
		return connect.NewError(connect.CodeFailedPrecondition, s.notHere(ctx, d))
	}
	h, err := s.history(d)
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(s.cfg.DataDir, "restore-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	full, plain := filepath.Join(tmp, "db.musq"), filepath.Join(tmp, "plain.musq")
	if err := h.Restore(at, full); err != nil {
		return err
	}
	if err := replication.PlainCopy(ctx, full, plain); err != nil {
		return err
	}
	p := &restorePoint{at: at}
	if p.full, err = sql.Open("sqlite", full); err != nil {
		return err
	}
	defer p.full.Close()
	if p.plain, err = sql.Open("sqlite", plain); err != nil {
		return err
	}
	defer p.plain.Close()
	for range restoreTries {
		if err = p.restore(ctx, db); !errors.Is(err, engine.ErrBusy) {
			return err
		}
	}
	return connect.NewError(connect.CodeAborted, fmt.Errorf("the database kept changing during the restore: %w", err))
}

// restorePoint is a crdt database as this node held it at a time: full is the
// restored file, bookkeeping included, and plain its plain copy.
type restorePoint struct {
	at          time.Time
	full, plain *sql.DB
	schema      [][]any // full's, once read
	heldChecked bool
}

func declined(format string, args ...any) error {
	return connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf(format, args...))
}

// restore is one attempt: checked, diffed and written in a transaction on a
// connection of db's own, which is closed after, so no client inherits the
// pragmas it sets.
func (p *restorePoint) restore(ctx context.Context, db *sql.DB) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Raw(func(any) error { return sqldriver.ErrBadConn })
	for _, q := range []string{`PRAGMA foreign_keys = OFF`, `PRAGMA ignore_check_constraints = ON`} {
		if _, err := conn.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if p.schema == nil {
		if p.schema, err = schemaRows(ctx, p.full); err != nil {
			return err
		}
	}
	now, err := schemaRows(ctx, tx)
	if err != nil {
		return err
	}
	if !slices.EqualFunc(now, p.schema, sameRow) {
		return declined("the schema changed since %s: restore into a new database", p.at.UTC().Format(time.RFC3339Nano))
	}
	if !p.heldChecked {
		if held, err := replication.Withheld(ctx, p.full); err != nil || len(held) > 0 {
			return heldErr(held, err, "at "+p.at.UTC().Format(time.RFC3339Nano))
		}
		p.heldChecked = true
	}
	// Read after the transaction's first: a commit since then fails the
	// transaction with ErrBusy, so what this saw is what the diff runs on.
	if held, err := replication.Withheld(ctx, db); err != nil || len(held) > 0 {
		return heldErr(held, err, "now")
	}
	tables, err := userTables(ctx, p.plain)
	if err != nil {
		return err
	}
	for _, t := range tables {
		if err := t.restore(ctx, p.plain, tx); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func heldErr(held []replication.WithheldRow, err error, when string) error {
	if err != nil {
		return err
	}
	return declined("a row of %s is withheld by a UNIQUE conflict %s, which a restore in place cannot reproduce: restore into a new database", held[0].Table, when)
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// schemaRows is a database's sqlite_schema less the engine's own objects.
func schemaRows(ctx context.Context, q querier) ([][]any, error) {
	return readRows(ctx, q, `SELECT type, name, tbl_name, sql FROM main.sqlite_schema WHERE name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY type, name`)
}

func readRows(ctx context.Context, q querier, query string) ([][]any, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]any
	for rows.Next() {
		r := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range r {
			ptrs[i] = &r[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// diffTable is how a restore names one table's rows: by key, replication's
// identity (the declared primary key, or the rowid), and the other columns
// that are stored, not generated.
type diffTable struct {
	name string
	key  []string
	cols []string
}

// userTables are a plain copy's tables, which are the user's only.
func userTables(ctx context.Context, plain *sql.DB) ([]*diffTable, error) {
	rows, err := readRows(ctx, plain, `SELECT name FROM main.sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []*diffTable
	for _, r := range rows {
		name := fmt.Sprint(r[0])
		t := &diffTable{name: "main." + quoteIdent(name)}
		var lower []string
		cols, err := plain.QueryContext(ctx, `SELECT name, pk, hidden FROM pragma_table_xinfo(?) ORDER BY pk, cid`, name)
		if err != nil {
			return nil, err
		}
		for cols.Next() {
			var col string
			var pk, hidden int64
			if err := cols.Scan(&col, &pk, &hidden); err != nil {
				cols.Close()
				return nil, err
			}
			lower = append(lower, strings.ToLower(col))
			switch {
			case hidden != 0:
			case pk > 0:
				t.key = append(t.key, quoteIdent(col))
			default:
				t.cols = append(t.cols, quoteIdent(col))
			}
		}
		cols.Close()
		if err := cols.Err(); err != nil {
			return nil, err
		}
		if t.key == nil {
			for _, alias := range []string{"rowid", "_rowid_", "oid"} {
				if !slices.Contains(lower, alias) {
					t.key = []string{alias}
					break
				}
			}
			if t.key == nil {
				return nil, fmt.Errorf("table %s names columns rowid, _rowid_ and oid: its rowids cannot be restored", name)
			}
		}
		out = append(out, t)
	}
	return out, nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// restore makes t's rows in tx what they are in plain: rows only in tx go,
// rows only in plain come back, and a row in both has its differing columns
// set. An update that fails on a UNIQUE value another changed row still holds
// is deleted and inserted instead, once every other update is done: then the
// table holds only rows as plain has them, which held every UNIQUE constraint.
func (t *diffTable) restore(ctx context.Context, plain *sql.DB, tx *sql.Tx) error {
	all := append(slices.Clone(t.key), t.cols...)
	q := "SELECT " + strings.Join(all, ", ") + " FROM " + t.name + " ORDER BY " + strings.Join(t.key, ", ")
	then, err := readRows(ctx, plain, q)
	if err != nil {
		return err
	}
	now, err := readRows(ctx, tx, q)
	if err != nil {
		return err
	}
	nk := len(t.key)
	at := make(map[string]int, len(then))
	for i, r := range then {
		at[keyOf(r[:nk])] = i
	}
	seen := make([]bool, len(then))
	var gone [][]any
	type change struct{ now, then []any }
	var changed []change
	for _, r := range now {
		i, ok := at[keyOf(r[:nk])]
		if !ok {
			gone = append(gone, r[:nk])
			continue
		}
		seen[i] = true
		if !sameRow(r[nk:], then[i][nk:]) {
			changed = append(changed, change{r, then[i]})
		}
	}
	where := " WHERE " + strings.Join(t.key, " = ? AND ") + " = ?"
	del := "DELETE FROM " + t.name + where
	for _, k := range gone {
		if _, err := tx.ExecContext(ctx, del, k...); err != nil {
			return err
		}
	}
	var redo [][]any
	for _, c := range changed {
		var set []string
		var args []any
		for j, col := range t.cols {
			if !sameValue(c.now[nk+j], c.then[nk+j]) {
				set = append(set, col+" = ?")
				args = append(args, c.then[nk+j])
			}
		}
		_, err := tx.ExecContext(ctx, "UPDATE "+t.name+" SET "+strings.Join(set, ", ")+where, append(args, c.then[:nk]...)...)
		if err != nil && !strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return err
		}
		if err != nil {
			redo = append(redo, c.then)
		}
	}
	for _, r := range redo {
		if _, err := tx.ExecContext(ctx, del, r[:nk]...); err != nil {
			return err
		}
	}
	ins := "INSERT INTO " + t.name + "(" + strings.Join(all, ", ") + ") VALUES (?" + strings.Repeat(", ?", len(all)-1) + ")"
	for i, r := range then {
		if !seen[i] {
			redo = append(redo, r)
		}
	}
	for _, r := range redo {
		if _, err := tx.ExecContext(ctx, ins, r...); err != nil {
			return err
		}
	}
	return nil
}

// keyOf is a key's identity as replication has it (EncodePK, which makes an
// integral REAL the INTEGER it equals, as a key compares).
func keyOf(vals []any) string {
	var b []byte
	for _, v := range vals {
		e := replication.EncodePK(v)
		b = binary.AppendUvarint(b, uint64(len(e)))
		b = append(b, e...)
	}
	return string(b)
}

func sameRow(a, b []any) bool { return slices.EqualFunc(a, b, sameValue) }

// sameValue is equality as capture decides a cell changed: type and value.
func sameValue(a, b any) bool {
	ab, aok := a.([]byte)
	bb, bok := b.([]byte)
	if aok || bok {
		return aok && bok && bytes.Equal(ab, bb)
	}
	return a == b
}
