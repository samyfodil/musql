package boost

import (
	"context"
	"database/sql"
	"fmt"
	"iter"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
)

// Querier is what QueryStruct runs on: a *DB or a *Tx.
type Querier interface {
	Query(ctx context.Context, query string, args ...any) (*Rows, error)
}

// QueryStruct runs query and yields each row scanned into a T, a struct
// whose fields take the result columns. It stops at the first error, which
// it yields with a zero T.
//
// A column goes to the exported field tagged `db:"<column>"`, or else to the
// one whose name matches it ignoring case; `db:"-"` leaves a field out. A
// column with no field, or a field with no column, is an error: a field left
// at its zero value would read as data. A pointer field is nil for NULL and
// otherwise points at a new value.
//
//	for u, err := range boost.QueryStruct[User](ctx, db, "SELECT id, name FROM users") {
//		...
//	}
func QueryStruct[T any](ctx context.Context, q Querier, query string, args ...any) iter.Seq2[T, error] {
	return func(yield func(T, error) bool) {
		var zero T
		rows, err := q.Query(ctx, query, args...)
		if err != nil {
			yield(zero, err)
			return
		}
		defer rows.Close()
		typ := reflect.TypeFor[T]()
		plan, err := planFor(typ, query, rows.Columns())
		if err != nil {
			yield(zero, err)
			return
		}
		var t T
		v := reflect.ValueOf(&t).Elem()
		for rows.Next() {
			t = zero
			if err := plan.scan(rows, v); err != nil {
				yield(zero, err)
				return
			}
			if !yield(t, nil) {
				return
			}
		}
	}
}

// QueryRowStruct runs query and scans its first row into a T, as
// QueryStruct describes, reporting sql.ErrNoRows when there is none.
func QueryRowStruct[T any](ctx context.Context, q Querier, query string, args ...any) (T, error) {
	for t, err := range QueryStruct[T](ctx, q, query, args...) {
		return t, err
	}
	var zero T
	return zero, sql.ErrNoRows
}

// structPlan maps result columns onto a struct's fields: fields[i] is the
// index of the field column i fills, and ptr[i] says it is a pointer.
type structPlan struct {
	cols   []string
	fields []int
	ptr    []bool
}

type planKey struct {
	typ   reflect.Type
	query string
}

// plans caches up to maxPlans plans and starts over past that, so SQL text
// built at run time cannot grow it without bound.
var (
	plans     sync.Map // planKey -> *structPlan
	planCount atomic.Int64
)

const maxPlans = 1024

// planFor returns the plan for typ and these columns, built once per type
// and query text and again only when the columns change.
func planFor(typ reflect.Type, query string, cols []string) (*structPlan, error) {
	key := planKey{typ, query}
	if v, ok := plans.Load(key); ok {
		if p := v.(*structPlan); slices.Equal(p.cols, cols) {
			return p, nil
		}
	}
	p, err := buildPlan(typ, cols)
	if err != nil {
		return nil, err
	}
	if planCount.Add(1) > maxPlans {
		plans.Clear()
		planCount.Store(1)
	}
	plans.Store(key, p)
	return p, nil
}

func buildPlan(typ reflect.Type, cols []string) (*structPlan, error) {
	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("boost: QueryStruct needs a struct type, not %s", typ)
	}
	byName := map[string]int{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		name := f.Tag.Get("db")
		if name == "-" || !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		key := strings.ToLower(name)
		if _, dup := byName[key]; dup {
			return nil, fmt.Errorf("boost: %s has two fields for column %q", typ, name)
		}
		byName[key] = i
	}
	p := &structPlan{cols: slices.Clone(cols), fields: make([]int, len(cols)), ptr: make([]bool, len(cols))}
	used := map[int]bool{}
	for i, c := range cols {
		idx, ok := byName[strings.ToLower(c)]
		if !ok {
			return nil, fmt.Errorf("boost: column %q has no field in %s", c, typ)
		}
		if used[idx] {
			return nil, fmt.Errorf("boost: column %q repeats a column already scanned into %s.%s", c, typ, typ.Field(idx).Name)
		}
		used[idx] = true
		p.fields[i] = idx
		p.ptr[i] = typ.Field(idx).Type.Kind() == reflect.Pointer
	}
	for _, idx := range byName {
		if !used[idx] {
			return nil, fmt.Errorf("boost: field %s.%s has no column in the result", typ, typ.Field(idx).Name)
		}
	}
	return p, nil
}

// scan fills v, a struct, from the current row.
func (p *structPlan) scan(rows *Rows, v reflect.Value) error {
	for i, idx := range p.fields {
		f := v.Field(idx)
		if p.ptr[i] {
			if rows.r.Null(rows.pos, i) {
				continue
			}
			f.Set(reflect.New(f.Type().Elem()))
		} else {
			f = f.Addr()
		}
		if err := rows.r.Scan(rows.pos, i, f.Interface()); err != nil {
			return fmt.Errorf("sql: Scan error on column %q into %s.%s: %w", p.cols[i], v.Type(), v.Type().Field(idx).Name, err)
		}
	}
	return nil
}
