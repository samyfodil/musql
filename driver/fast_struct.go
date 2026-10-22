package driver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/samyfodil/musql/engine"
)

// QueryRowStruct runs s and scans its first row into a T, a struct whose
// fields take the result columns, returning sql.ErrNoRows when there is none.
//
// A column goes to the exported field tagged `db:"<column>"`, or else to the
// one whose name matches it ignoring case; `db:"-"` leaves a field out. A
// column with no field, or a field with no column, is an error: a field left
// at its zero value would read as data. Each field takes the destinations
// FastStmt.Query's scan does.
func QueryRowStruct[T any](ctx context.Context, s *FastStmt, args []any) (T, error) {
	var out T
	found := false
	err := QueryStruct(ctx, s, args, func(t *T) error {
		out, found = *t, true
		return errStopRows
	})
	if err != nil && !errors.Is(err, errStopRows) {
		return out, err
	}
	if !found {
		return out, sql.ErrNoRows
	}
	return out, nil
}

// QueryStruct runs s and calls each once per row with the row scanned into a
// T, as QueryRowStruct describes. The *T is reused between rows.
func QueryStruct[T any](ctx context.Context, s *FastStmt, args []any, each func(*T) error) error {
	typ := reflect.TypeFor[T]()
	var t T
	rv := reflect.ValueOf(&t).Elem()
	return s.run(ctx, args, func(r *Rows) error {
		plan, err := s.structPlan(typ, r.cols)
		if err != nil {
			return err
		}
		for _, row := range r.rows {
			for i, idx := range plan.fields {
				f := rv.Field(idx)
				if plan.ptr[i] {
					if row[i].Typ == engine.Null {
						f.SetZero()
						continue
					}
					f.Set(reflect.New(f.Type().Elem()))
				} else {
					f = f.Addr()
				}
				if err := r.scanFast(i, row[i], f.Interface()); err != nil {
					return fmt.Errorf("sql: Scan error on column %q into %s.%s: %w", r.cols[i], typ, typ.Field(idx).Name, err)
				}
			}
			if err := each(&t); err != nil {
				return err
			}
		}
		return nil
	})
}

// fastStructPlan maps a statement's result columns onto a struct's fields:
// fields[i] is the index of the field column i fills, and ptr[i] says that
// field is a pointer, nil for NULL and otherwise pointing at a new value.
type fastStructPlan struct {
	typ    reflect.Type
	cols   []string
	fields []int
	ptr    []bool
}

// structPlan returns the plan for typ and these columns, building it once and
// again only when either changes.
func (s *FastStmt) structPlan(typ reflect.Type, cols []string) (*fastStructPlan, error) {
	if p := s.plan; p != nil && p.typ == typ && slices.Equal(p.cols, cols) {
		return p, nil
	}
	if typ.Kind() != reflect.Struct {
		return nil, fmt.Errorf("driver: QueryStruct needs a struct type, not %s", typ)
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
			return nil, fmt.Errorf("driver: %s has two fields for column %q", typ, name)
		}
		byName[key] = i
	}
	p := &fastStructPlan{typ: typ, cols: slices.Clone(cols), fields: make([]int, len(cols)), ptr: make([]bool, len(cols))}
	used := map[int]bool{}
	for i, c := range cols {
		idx, ok := byName[strings.ToLower(c)]
		if !ok {
			return nil, fmt.Errorf("driver: column %q has no field in %s", c, typ)
		}
		if used[idx] {
			return nil, fmt.Errorf("driver: column %q repeats a column already scanned into %s.%s", c, typ, typ.Field(idx).Name)
		}
		used[idx] = true
		p.fields[i] = idx
		p.ptr[i] = typ.Field(idx).Type.Kind() == reflect.Pointer
	}
	for _, idx := range byName {
		if !used[idx] {
			return nil, fmt.Errorf("driver: field %s.%s has no column in the result", typ, typ.Field(idx).Name)
		}
	}
	s.plan = p
	return p, nil
}
