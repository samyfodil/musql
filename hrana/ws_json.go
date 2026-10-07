package hrana

import (
	pbws "github.com/samyfodil/musql/hrana/gen/hrana/ws"
)

// JSON encoding of Hrana over WebSocket messages (subprotocols hrana1, hrana2
// and hrana3), mapped to and from the generated messages.

type jWSRequest struct {
	Type     string  `json:"type"`
	StreamID int32   `json:"stream_id"`
	CursorID int32   `json:"cursor_id"`
	MaxCount uint32  `json:"max_count"`
	Stmt     *jStmt  `json:"stmt"`
	Batch    *jBatch `json:"batch"`
	SQL      *string `json:"sql"`
	SQLID    *int32  `json:"sql_id"`
}

type jClientMsg struct {
	Type      string      `json:"type"`
	JWT       *string     `json:"jwt"`
	RequestID int32       `json:"request_id"`
	Request   *jWSRequest `json:"request"`
}

func (j *jClientMsg) proto() (*pbws.ClientMsg, error) {
	switch j.Type {
	case "hello":
		return &pbws.ClientMsg{Msg: &pbws.ClientMsg_Hello{Hello: &pbws.HelloMsg{Jwt: j.JWT}}}, nil
	case "request":
		if j.Request == nil {
			return nil, protoErr("request without a body")
		}
		rq, err := j.Request.proto()
		if err != nil {
			return nil, err
		}
		rq.RequestId = j.RequestID
		return &pbws.ClientMsg{Msg: &pbws.ClientMsg_Request{Request: rq}}, nil
	}
	return nil, protoErr("unknown message type " + j.Type)
}

func (j *jWSRequest) proto() (*pbws.RequestMsg, error) {
	r := &pbws.RequestMsg{}
	switch j.Type {
	case "open_stream":
		r.Request = &pbws.RequestMsg_OpenStream{OpenStream: &pbws.OpenStreamReq{StreamId: j.StreamID}}
	case "close_stream":
		r.Request = &pbws.RequestMsg_CloseStream{CloseStream: &pbws.CloseStreamReq{StreamId: j.StreamID}}
	case "execute":
		st, err := j.Stmt.proto()
		if err != nil {
			return nil, err
		}
		r.Request = &pbws.RequestMsg_Execute{Execute: &pbws.ExecuteReq{StreamId: j.StreamID, Stmt: st}}
	case "batch":
		b, err := j.Batch.proto()
		if err != nil {
			return nil, err
		}
		r.Request = &pbws.RequestMsg_Batch{Batch: &pbws.BatchReq{StreamId: j.StreamID, Batch: b}}
	case "open_cursor":
		b, err := j.Batch.proto()
		if err != nil {
			return nil, err
		}
		r.Request = &pbws.RequestMsg_OpenCursor{OpenCursor: &pbws.OpenCursorReq{StreamId: j.StreamID, CursorId: j.CursorID, Batch: b}}
	case "close_cursor":
		r.Request = &pbws.RequestMsg_CloseCursor{CloseCursor: &pbws.CloseCursorReq{CursorId: j.CursorID}}
	case "fetch_cursor":
		r.Request = &pbws.RequestMsg_FetchCursor{FetchCursor: &pbws.FetchCursorReq{CursorId: j.CursorID, MaxCount: j.MaxCount}}
	case "sequence":
		r.Request = &pbws.RequestMsg_Sequence{Sequence: &pbws.SequenceReq{StreamId: j.StreamID, Sql: j.SQL, SqlId: j.SQLID}}
	case "describe":
		r.Request = &pbws.RequestMsg_Describe{Describe: &pbws.DescribeReq{StreamId: j.StreamID, Sql: j.SQL, SqlId: j.SQLID}}
	case "store_sql":
		if j.SQL == nil || j.SQLID == nil {
			return nil, protoErr("store_sql needs sql and sql_id")
		}
		r.Request = &pbws.RequestMsg_StoreSql{StoreSql: &pbws.StoreSqlReq{SqlId: *j.SQLID, Sql: *j.SQL}}
	case "close_sql":
		if j.SQLID == nil {
			return nil, protoErr("close_sql needs sql_id")
		}
		r.Request = &pbws.RequestMsg_CloseSql{CloseSql: &pbws.CloseSqlReq{SqlId: *j.SQLID}}
	case "get_autocommit":
		r.Request = &pbws.RequestMsg_GetAutocommit{GetAutocommit: &pbws.GetAutocommitReq{StreamId: j.StreamID}}
	default:
		return nil, protoErr("unknown request type " + j.Type)
	}
	return r, nil
}

func jsonServerMsg(m *pbws.ServerMsg) map[string]any {
	switch x := m.Msg.(type) {
	case *pbws.ServerMsg_HelloOk:
		return map[string]any{"type": "hello_ok"}
	case *pbws.ServerMsg_HelloError:
		return map[string]any{"type": "hello_error", "error": jsonError(x.HelloError.GetError())}
	case *pbws.ServerMsg_ResponseError:
		return map[string]any{"type": "response_error", "request_id": x.ResponseError.RequestId, "error": jsonError(x.ResponseError.GetError())}
	case *pbws.ServerMsg_ResponseOk:
		return map[string]any{"type": "response_ok", "request_id": x.ResponseOk.RequestId, "response": jsonWSResponse(x.ResponseOk)}
	}
	return map[string]any{"type": "response_error", "error": map[string]any{"message": "unknown server message"}}
}

func jsonWSResponse(r *pbws.ResponseOkMsg) map[string]any {
	switch x := r.Response.(type) {
	case *pbws.ResponseOkMsg_OpenStream:
		return map[string]any{"type": "open_stream"}
	case *pbws.ResponseOkMsg_CloseStream:
		return map[string]any{"type": "close_stream"}
	case *pbws.ResponseOkMsg_Execute:
		return map[string]any{"type": "execute", "result": jsonStmtResult(x.Execute.GetResult())}
	case *pbws.ResponseOkMsg_Batch:
		res := x.Batch.GetResult()
		n := 0
		for k := range res.StepResults {
			n = max(n, int(k)+1)
		}
		for k := range res.StepErrors {
			n = max(n, int(k)+1)
		}
		return map[string]any{"type": "batch", "result": jsonBatchResult(res, n)}
	case *pbws.ResponseOkMsg_OpenCursor:
		return map[string]any{"type": "open_cursor"}
	case *pbws.ResponseOkMsg_CloseCursor:
		return map[string]any{"type": "close_cursor"}
	case *pbws.ResponseOkMsg_FetchCursor:
		entries := make([]any, len(x.FetchCursor.Entries))
		for i, e := range x.FetchCursor.Entries {
			entries[i] = jsonCursorEntry(e)
		}
		return map[string]any{"type": "fetch_cursor", "entries": entries, "done": x.FetchCursor.Done}
	case *pbws.ResponseOkMsg_Sequence:
		return map[string]any{"type": "sequence"}
	case *pbws.ResponseOkMsg_Describe:
		return map[string]any{"type": "describe", "result": jsonDescribe(x.Describe.GetResult())}
	case *pbws.ResponseOkMsg_StoreSql:
		return map[string]any{"type": "store_sql"}
	case *pbws.ResponseOkMsg_CloseSql:
		return map[string]any{"type": "close_sql"}
	case *pbws.ResponseOkMsg_GetAutocommit:
		return map[string]any{"type": "get_autocommit", "is_autocommit": x.GetAutocommit.IsAutocommit}
	}
	return map[string]any{"type": "unknown"}
}
