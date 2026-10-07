package hrana

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	pb "github.com/samyfodil/musql/hrana/gen/hrana"
	pbhttp "github.com/samyfodil/musql/hrana/gen/hrana/http"
)

// Hrana's JSON encoding, the canonical one. It is not protojson: unions are
// objects with a "type" field, integers travel as strings and blobs as base64.
// These types mirror the spec's TypeScript definitions and convert to and from
// the generated protobuf messages, which the server uses throughout.

type jValue struct {
	Type   string          `json:"type"`
	Value  json.RawMessage `json:"value,omitempty"`
	Base64 string          `json:"base64,omitempty"`
}

func (j jValue) proto() (*pb.Value, error) {
	switch j.Type {
	case "null":
		return &pb.Value{Value: &pb.Value_Null_{Null: &pb.Value_Null{}}}, nil
	case "integer":
		var s string
		if err := json.Unmarshal(j.Value, &s); err != nil {
			var n int64 // tolerate a bare number
			if err := json.Unmarshal(j.Value, &n); err != nil {
				return nil, protoErr("integer value must be a string")
			}
			return &pb.Value{Value: &pb.Value_Integer{Integer: n}}, nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, protoErr("invalid integer " + s)
		}
		return &pb.Value{Value: &pb.Value_Integer{Integer: n}}, nil
	case "float":
		var f float64
		if err := json.Unmarshal(j.Value, &f); err != nil {
			return nil, protoErr("invalid float")
		}
		return &pb.Value{Value: &pb.Value_Float{Float: f}}, nil
	case "text":
		var s string
		if err := json.Unmarshal(j.Value, &s); err != nil {
			return nil, protoErr("invalid text")
		}
		return &pb.Value{Value: &pb.Value_Text{Text: s}}, nil
	case "blob":
		b, err := base64.StdEncoding.DecodeString(j.Base64)
		if err != nil {
			if b, err = base64.RawStdEncoding.DecodeString(j.Base64); err != nil {
				return nil, protoErr("invalid base64 blob")
			}
		}
		return &pb.Value{Value: &pb.Value_Blob{Blob: b}}, nil
	}
	return nil, protoErr("unknown value type " + j.Type)
}

func jsonValue(v *pb.Value) any {
	switch x := v.GetValue().(type) {
	case *pb.Value_Integer:
		return map[string]any{"type": "integer", "value": strconv.FormatInt(x.Integer, 10)}
	case *pb.Value_Float:
		if math.IsInf(x.Float, 0) || math.IsNaN(x.Float) {
			return map[string]any{"type": "null"} // JSON has no Inf or NaN
		}
		return map[string]any{"type": "float", "value": x.Float}
	case *pb.Value_Text:
		return map[string]any{"type": "text", "value": x.Text}
	case *pb.Value_Blob:
		return map[string]any{"type": "blob", "base64": base64.StdEncoding.EncodeToString(x.Blob)}
	}
	return map[string]any{"type": "null"}
}

type jNamedArg struct {
	Name  string `json:"name"`
	Value jValue `json:"value"`
}

type jStmt struct {
	SQL       *string     `json:"sql"`
	SQLID     *int32      `json:"sql_id"`
	Args      []jValue    `json:"args"`
	NamedArgs []jNamedArg `json:"named_args"`
	WantRows  *bool       `json:"want_rows"`
}

func (j *jStmt) proto() (*pb.Stmt, error) {
	if j == nil {
		return nil, protoErr("missing stmt")
	}
	st := &pb.Stmt{Sql: j.SQL, SqlId: j.SQLID, WantRows: j.WantRows}
	for _, a := range j.Args {
		v, err := a.proto()
		if err != nil {
			return nil, err
		}
		st.Args = append(st.Args, v)
	}
	for _, na := range j.NamedArgs {
		v, err := na.Value.proto()
		if err != nil {
			return nil, err
		}
		st.NamedArgs = append(st.NamedArgs, &pb.NamedArg{Name: na.Name, Value: v})
	}
	return st, nil
}

type jCond struct {
	Type  string   `json:"type"`
	Step  uint32   `json:"step"`
	Cond  *jCond   `json:"cond"`
	Conds []*jCond `json:"conds"`
}

func (j *jCond) proto() (*pb.BatchCond, error) {
	if j == nil {
		return nil, protoErr("missing condition")
	}
	list := func() (*pb.BatchCond_CondList, error) {
		l := &pb.BatchCond_CondList{}
		for _, c := range j.Conds {
			pc, err := c.proto()
			if err != nil {
				return nil, err
			}
			l.Conds = append(l.Conds, pc)
		}
		return l, nil
	}
	switch j.Type {
	case "ok":
		return &pb.BatchCond{Cond: &pb.BatchCond_StepOk{StepOk: j.Step}}, nil
	case "error":
		return &pb.BatchCond{Cond: &pb.BatchCond_StepError{StepError: j.Step}}, nil
	case "not":
		c, err := j.Cond.proto()
		if err != nil {
			return nil, err
		}
		return &pb.BatchCond{Cond: &pb.BatchCond_Not{Not: c}}, nil
	case "and":
		l, err := list()
		if err != nil {
			return nil, err
		}
		return &pb.BatchCond{Cond: &pb.BatchCond_And{And: l}}, nil
	case "or":
		l, err := list()
		if err != nil {
			return nil, err
		}
		return &pb.BatchCond{Cond: &pb.BatchCond_Or{Or: l}}, nil
	case "is_autocommit":
		return &pb.BatchCond{Cond: &pb.BatchCond_IsAutocommit_{IsAutocommit: &pb.BatchCond_IsAutocommit{}}}, nil
	}
	return nil, protoErr("unknown condition type " + j.Type)
}

type jBatch struct {
	Steps []struct {
		Condition *jCond `json:"condition"`
		Stmt      *jStmt `json:"stmt"`
	} `json:"steps"`
}

func (j *jBatch) proto() (*pb.Batch, error) {
	if j == nil {
		return nil, protoErr("missing batch")
	}
	b := &pb.Batch{}
	for _, s := range j.Steps {
		st, err := s.Stmt.proto()
		if err != nil {
			return nil, err
		}
		step := &pb.BatchStep{Stmt: st}
		if s.Condition != nil {
			if step.Condition, err = s.Condition.proto(); err != nil {
				return nil, err
			}
		}
		b.Steps = append(b.Steps, step)
	}
	return b, nil
}

type jStreamRequest struct {
	Type  string  `json:"type"`
	Stmt  *jStmt  `json:"stmt"`
	Batch *jBatch `json:"batch"`
	SQL   *string `json:"sql"`
	SQLID *int32  `json:"sql_id"`
}

func (j *jStreamRequest) proto() (*pbhttp.StreamRequest, error) {
	r := &pbhttp.StreamRequest{}
	switch j.Type {
	case "close":
		r.Request = &pbhttp.StreamRequest_Close{Close: &pbhttp.CloseStreamReq{}}
	case "execute":
		st, err := j.Stmt.proto()
		if err != nil {
			return nil, err
		}
		r.Request = &pbhttp.StreamRequest_Execute{Execute: &pbhttp.ExecuteStreamReq{Stmt: st}}
	case "batch":
		b, err := j.Batch.proto()
		if err != nil {
			return nil, err
		}
		r.Request = &pbhttp.StreamRequest_Batch{Batch: &pbhttp.BatchStreamReq{Batch: b}}
	case "sequence":
		r.Request = &pbhttp.StreamRequest_Sequence{Sequence: &pbhttp.SequenceStreamReq{Sql: j.SQL, SqlId: j.SQLID}}
	case "describe":
		r.Request = &pbhttp.StreamRequest_Describe{Describe: &pbhttp.DescribeStreamReq{Sql: j.SQL, SqlId: j.SQLID}}
	case "store_sql":
		if j.SQL == nil || j.SQLID == nil {
			return nil, protoErr("store_sql needs sql and sql_id")
		}
		r.Request = &pbhttp.StreamRequest_StoreSql{StoreSql: &pbhttp.StoreSqlStreamReq{SqlId: *j.SQLID, Sql: *j.SQL}}
	case "close_sql":
		if j.SQLID == nil {
			return nil, protoErr("close_sql needs sql_id")
		}
		r.Request = &pbhttp.StreamRequest_CloseSql{CloseSql: &pbhttp.CloseSqlStreamReq{SqlId: *j.SQLID}}
	case "get_autocommit":
		r.Request = &pbhttp.StreamRequest_GetAutocommit{GetAutocommit: &pbhttp.GetAutocommitStreamReq{}}
	default:
		return nil, protoErr("unknown request type " + j.Type)
	}
	return r, nil
}

type jPipelineReq struct {
	Baton    *string          `json:"baton"`
	Requests []jStreamRequest `json:"requests"`
}

type jCursorReq struct {
	Baton *string `json:"baton"`
	Batch *jBatch `json:"batch"`
}

// ---- responses ----

func jsonError(e *pb.Error) map[string]any {
	m := map[string]any{"message": e.GetMessage()}
	if e.Code != nil {
		m["code"] = *e.Code
	}
	return m
}

func jsonCols(cols []*pb.Col) []any {
	out := make([]any, len(cols))
	for i, c := range cols {
		out[i] = map[string]any{"name": c.Name, "decltype": c.Decltype}
	}
	return out
}

func jsonRow(r *pb.Row) []any {
	out := make([]any, len(r.GetValues()))
	for i, v := range r.GetValues() {
		out[i] = jsonValue(v)
	}
	return out
}

func jsonRowid(id *int64) any {
	if id == nil {
		return nil
	}
	return strconv.FormatInt(*id, 10)
}

func jsonStmtResult(r *pb.StmtResult) map[string]any {
	rows := make([]any, len(r.GetRows()))
	for i, row := range r.GetRows() {
		rows[i] = jsonRow(row)
	}
	return map[string]any{
		"cols":               jsonCols(r.GetCols()),
		"rows":               rows,
		"affected_row_count": r.GetAffectedRowCount(),
		"last_insert_rowid":  jsonRowid(r.LastInsertRowid),
		"rows_read":          0,
		"rows_written":       0,
		"query_duration_ms":  0,
	}
}

func jsonBatchResult(r *pb.BatchResult, n int) map[string]any {
	results := make([]any, n)
	errs := make([]any, n)
	for i := range n {
		if res, ok := r.StepResults[uint32(i)]; ok {
			results[i] = jsonStmtResult(res)
		}
		if e, ok := r.StepErrors[uint32(i)]; ok {
			errs[i] = jsonError(e)
		}
	}
	return map[string]any{"step_results": results, "step_errors": errs}
}

func jsonDescribe(r *pb.DescribeResult) map[string]any {
	params := make([]any, len(r.GetParams()))
	for i, p := range r.GetParams() {
		params[i] = map[string]any{"name": p.Name}
	}
	cols := make([]any, len(r.GetCols()))
	for i, c := range r.GetCols() {
		cols[i] = map[string]any{"name": c.Name, "decltype": c.Decltype}
	}
	return map[string]any{"params": params, "cols": cols, "is_explain": r.IsExplain, "is_readonly": r.IsReadonly}
}

// jsonStreamResult encodes one pipeline result; n is the request's batch length
// (step arrays are as long as the batch, with null for skipped steps).
func jsonStreamResult(r *pbhttp.StreamResult, n int) map[string]any {
	if e := r.GetError(); e != nil {
		return map[string]any{"type": "error", "error": jsonError(e)}
	}
	resp := r.GetOk()
	var body map[string]any
	switch x := resp.GetResponse().(type) {
	case *pbhttp.StreamResponse_Close:
		body = map[string]any{"type": "close"}
	case *pbhttp.StreamResponse_Execute:
		body = map[string]any{"type": "execute", "result": jsonStmtResult(x.Execute.GetResult())}
	case *pbhttp.StreamResponse_Batch:
		body = map[string]any{"type": "batch", "result": jsonBatchResult(x.Batch.GetResult(), n)}
	case *pbhttp.StreamResponse_Sequence:
		body = map[string]any{"type": "sequence"}
	case *pbhttp.StreamResponse_Describe:
		body = map[string]any{"type": "describe", "result": jsonDescribe(x.Describe.GetResult())}
	case *pbhttp.StreamResponse_StoreSql:
		body = map[string]any{"type": "store_sql"}
	case *pbhttp.StreamResponse_CloseSql:
		body = map[string]any{"type": "close_sql"}
	case *pbhttp.StreamResponse_GetAutocommit:
		body = map[string]any{"type": "get_autocommit", "is_autocommit": x.GetAutocommit.GetIsAutocommit()}
	default:
		return map[string]any{"type": "error", "error": map[string]any{"message": fmt.Sprintf("unhandled response %T", x)}}
	}
	return map[string]any{"type": "ok", "response": body}
}

func jsonCursorEntry(e *pb.CursorEntry) map[string]any {
	switch x := e.GetEntry().(type) {
	case *pb.CursorEntry_StepBegin:
		return map[string]any{"type": "step_begin", "step": x.StepBegin.GetStep(), "cols": jsonCols(x.StepBegin.GetCols())}
	case *pb.CursorEntry_StepEnd:
		return map[string]any{"type": "step_end", "affected_row_count": x.StepEnd.GetAffectedRowCount(),
			"last_insert_rowid": jsonRowid(x.StepEnd.LastInsertRowid)}
	case *pb.CursorEntry_StepError:
		return map[string]any{"type": "step_error", "step": x.StepError.GetStep(), "error": jsonError(x.StepError.GetError())}
	case *pb.CursorEntry_Row:
		return map[string]any{"type": "row", "row": jsonRow(x.Row)}
	case *pb.CursorEntry_Error:
		return map[string]any{"type": "error", "error": jsonError(x.Error)}
	}
	return map[string]any{"type": "error", "error": map[string]any{"message": "unknown cursor entry"}}
}
