package hrana

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"sync"

	"github.com/coder/websocket"
	"google.golang.org/protobuf/proto"

	pb "github.com/samyfodil/musql/gen/hrana"
	pbws "github.com/samyfodil/musql/gen/hrana/ws"
)

// Hrana over WebSocket. Each WebSocket connection hosts any number of streams;
// requests on one stream run in order on that stream's own goroutine, so a
// transaction waiting on one stream never blocks another. Stored SQL texts are
// per connection.

var wsSubprotocols = []string{"hrana3-protobuf", "hrana3", "hrana2", "hrana1"}

func isWebSocket(r *http.Request) bool {
	return r.Header.Get("Upgrade") != "" && headerHasToken(r.Header.Get("Connection"), "upgrade")
}

func headerHasToken(h, tok string) bool {
	for _, part := range strings.Split(h, ",") {
		if strings.EqualFold(strings.TrimSpace(part), tok) {
			return true
		}
	}
	return false
}

type wsConn struct {
	srv      *Server
	c        *websocket.Conn
	protobuf bool
	ctx      context.Context

	wmu sync.Mutex // one writer at a time

	mu      sync.Mutex
	authed  bool
	streams map[int32]*wsStream
	cursors map[int32]*wsCursor
	sqls    *sqlStore
}

type wsStream struct {
	*stream
	open bool // false when open_stream failed; requests on it then fail
	jobs chan func()
}

type wsCursor struct {
	streamID int32
	entries  []*pb.CursorEntry
	err      *pb.Error
}

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	if !s.track() {
		http.Error(w, `{"message":"server is closing"}`, http.StatusServiceUnavailable)
		return
	}
	defer s.ws.Done()
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: wsSubprotocols, InsecureSkipVerify: true})
	if err != nil {
		return
	}
	c.SetReadLimit(64 << 20)
	sub := c.Subprotocol()
	if sub == "" {
		c.Close(websocket.StatusProtocolError, "no supported hrana subprotocol")
		return
	}
	ctx, cancel := context.WithCancel(s.base)
	wc := &wsConn{srv: s, c: c, protobuf: sub == "hrana3-protobuf", ctx: ctx,
		authed: s.authToken == "", streams: map[int32]*wsStream{}, cursors: map[int32]*wsCursor{}, sqls: newSQLStore()}
	defer func() {
		cancel()
		wc.closeAll()
		c.CloseNow()
	}()
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		if (typ == websocket.MessageBinary) != wc.protobuf {
			c.Close(websocket.StatusUnsupportedData, "frame type does not match the negotiated encoding")
			return
		}
		msg, err := wc.decode(data)
		if err != nil {
			c.Close(websocket.StatusProtocolError, err.Error())
			return
		}
		if !wc.dispatch(msg) {
			return
		}
	}
}

func (wc *wsConn) closeAll() {
	wc.mu.Lock()
	defer wc.mu.Unlock()
	for id, st := range wc.streams {
		close(st.jobs)
		delete(wc.streams, id)
	}
}

// dispatch handles one client message; false ends the connection.
func (wc *wsConn) dispatch(msg *pbws.ClientMsg) bool {
	switch m := msg.Msg.(type) {
	case *pbws.ClientMsg_Hello:
		if wc.srv.authToken != "" && subtle.ConstantTimeCompare([]byte(m.Hello.GetJwt()), []byte(wc.srv.authToken)) != 1 {
			wc.send(&pbws.ServerMsg{Msg: &pbws.ServerMsg_HelloError{HelloError: &pbws.HelloErrorMsg{
				Error: &pb.Error{Message: "authentication failed", Code: ptr("UNAUTHORIZED")}}}})
			wc.c.Close(websocket.StatusPolicyViolation, "authentication failed")
			return false
		}
		wc.mu.Lock()
		wc.authed = true
		wc.mu.Unlock()
		wc.send(&pbws.ServerMsg{Msg: &pbws.ServerMsg_HelloOk{HelloOk: &pbws.HelloOkMsg{}}})
		return true
	case *pbws.ClientMsg_Request:
		wc.mu.Lock()
		authed := wc.authed
		wc.mu.Unlock()
		if !authed {
			wc.c.Close(websocket.StatusPolicyViolation, "request before a successful hello")
			return false
		}
		wc.request(m.Request)
		return true
	}
	wc.c.Close(websocket.StatusProtocolError, "unknown message")
	return false
}

func (wc *wsConn) reply(id int32, resp *pbws.ResponseOkMsg) {
	resp.RequestId = id
	wc.send(&pbws.ServerMsg{Msg: &pbws.ServerMsg_ResponseOk{ResponseOk: resp}})
}

func (wc *wsConn) replyErr(id int32, err error) {
	wc.send(&pbws.ServerMsg{Msg: &pbws.ServerMsg_ResponseError{ResponseError: &pbws.ResponseErrorMsg{RequestId: id, Error: toError(err)}}})
}

// onStream queues fn on a stream's goroutine, or answers an error when the
// stream does not exist.
func (wc *wsConn) onStream(reqID, streamID int32, fn func(st *wsStream)) {
	wc.mu.Lock()
	st, ok := wc.streams[streamID]
	wc.mu.Unlock()
	if !ok {
		wc.replyErr(reqID, protoErr("no such stream"))
		return
	}
	st.jobs <- func() {
		if !st.open {
			wc.replyErr(reqID, protoErr("the stream failed to open"))
			return
		}
		fn(st)
	}
}

func (wc *wsConn) request(rq *pbws.RequestMsg) {
	id := rq.RequestId
	switch x := rq.Request.(type) {
	case *pbws.RequestMsg_OpenStream:
		sid := x.OpenStream.StreamId
		wc.mu.Lock()
		if _, dup := wc.streams[sid]; dup {
			wc.mu.Unlock()
			wc.replyErr(id, protoErr("stream id already in use"))
			return
		}
		st := &wsStream{jobs: make(chan func(), 64)}
		wc.streams[sid] = st
		wc.mu.Unlock()
		wc.srv.ws.Add(1) // under the connection, which Close is already waiting on
		go func() {
			defer wc.srv.ws.Done()
			for job := range st.jobs {
				job()
			}
			if st.stream != nil {
				st.close()
			}
		}()
		st.jobs <- func() {
			s, err := newStream(wc.ctx, wc.srv.db, wc.sqls)
			if err != nil {
				wc.replyErr(id, err)
				return
			}
			st.stream, st.open = s, true
			wc.reply(id, &pbws.ResponseOkMsg{Response: &pbws.ResponseOkMsg_OpenStream{OpenStream: &pbws.OpenStreamResp{}}})
		}
	case *pbws.RequestMsg_CloseStream:
		sid := x.CloseStream.StreamId
		wc.mu.Lock()
		st, ok := wc.streams[sid]
		delete(wc.streams, sid)
		wc.mu.Unlock()
		if !ok {
			wc.reply(id, &pbws.ResponseOkMsg{Response: &pbws.ResponseOkMsg_CloseStream{CloseStream: &pbws.CloseStreamResp{}}})
			return
		}
		st.jobs <- func() {
			wc.reply(id, &pbws.ResponseOkMsg{Response: &pbws.ResponseOkMsg_CloseStream{CloseStream: &pbws.CloseStreamResp{}}})
		}
		close(st.jobs)
	case *pbws.RequestMsg_Execute:
		wc.onStream(id, x.Execute.StreamId, func(st *wsStream) {
			r, err := st.execute(wc.ctx, x.Execute.GetStmt())
			if err != nil {
				wc.replyErr(id, err)
				return
			}
			wc.reply(id, &pbws.ResponseOkMsg{Response: &pbws.ResponseOkMsg_Execute{Execute: &pbws.ExecuteResp{Result: r}}})
		})
	case *pbws.RequestMsg_Batch:
		wc.onStream(id, x.Batch.StreamId, func(st *wsStream) {
			r := st.batch(wc.ctx, x.Batch.GetBatch())
			wc.reply(id, &pbws.ResponseOkMsg{Response: &pbws.ResponseOkMsg_Batch{Batch: &pbws.BatchResp{Result: r}}})
		})
	case *pbws.RequestMsg_Sequence:
		wc.onStream(id, x.Sequence.StreamId, func(st *wsStream) {
			if err := st.sequence(wc.ctx, x.Sequence.Sql, x.Sequence.SqlId); err != nil {
				wc.replyErr(id, err)
				return
			}
			wc.reply(id, &pbws.ResponseOkMsg{Response: &pbws.ResponseOkMsg_Sequence{Sequence: &pbws.SequenceResp{}}})
		})
	case *pbws.RequestMsg_Describe:
		wc.onStream(id, x.Describe.StreamId, func(st *wsStream) {
			r, err := st.describe(wc.ctx, x.Describe.Sql, x.Describe.SqlId)
			if err != nil {
				wc.replyErr(id, err)
				return
			}
			wc.reply(id, &pbws.ResponseOkMsg{Response: &pbws.ResponseOkMsg_Describe{Describe: &pbws.DescribeResp{Result: r}}})
		})
	case *pbws.RequestMsg_GetAutocommit:
		wc.onStream(id, x.GetAutocommit.StreamId, func(st *wsStream) {
			wc.reply(id, &pbws.ResponseOkMsg{Response: &pbws.ResponseOkMsg_GetAutocommit{
				GetAutocommit: &pbws.GetAutocommitResp{IsAutocommit: st.isAutocommit()}}})
		})
	case *pbws.RequestMsg_StoreSql:
		wc.sqls.put(x.StoreSql.SqlId, x.StoreSql.Sql)
		wc.reply(id, &pbws.ResponseOkMsg{Response: &pbws.ResponseOkMsg_StoreSql{StoreSql: &pbws.StoreSqlResp{}}})
	case *pbws.RequestMsg_CloseSql:
		wc.sqls.del(x.CloseSql.SqlId)
		wc.reply(id, &pbws.ResponseOkMsg{Response: &pbws.ResponseOkMsg_CloseSql{CloseSql: &pbws.CloseSqlResp{}}})
	case *pbws.RequestMsg_OpenCursor:
		cid := x.OpenCursor.CursorId
		cur := &wsCursor{streamID: x.OpenCursor.StreamId}
		wc.mu.Lock()
		wc.cursors[cid] = cur
		wc.mu.Unlock()
		wc.onStream(id, cur.streamID, func(st *wsStream) {
			b := x.OpenCursor.GetBatch()
			cur.entries = cursorEntries(b, st.batch(wc.ctx, b))
			wc.reply(id, &pbws.ResponseOkMsg{Response: &pbws.ResponseOkMsg_OpenCursor{OpenCursor: &pbws.OpenCursorResp{}}})
		})
	case *pbws.RequestMsg_FetchCursor:
		cid, max := x.FetchCursor.CursorId, int(x.FetchCursor.MaxCount)
		wc.mu.Lock()
		cur, ok := wc.cursors[cid]
		wc.mu.Unlock()
		if !ok {
			wc.replyErr(id, protoErr("no such cursor"))
			return
		}
		// On the cursor's stream, so a fetch sent right after open_cursor waits
		// for the batch to finish.
		wc.onStream(id, cur.streamID, func(*wsStream) {
			n := min(max, len(cur.entries))
			if max <= 0 {
				n = len(cur.entries)
			}
			out := cur.entries[:n]
			cur.entries = cur.entries[n:]
			wc.reply(id, &pbws.ResponseOkMsg{Response: &pbws.ResponseOkMsg_FetchCursor{FetchCursor: &pbws.FetchCursorResp{
				Entries: out, Done: len(cur.entries) == 0}}})
		})
	case *pbws.RequestMsg_CloseCursor:
		wc.mu.Lock()
		delete(wc.cursors, x.CloseCursor.CursorId)
		wc.mu.Unlock()
		wc.reply(id, &pbws.ResponseOkMsg{Response: &pbws.ResponseOkMsg_CloseCursor{CloseCursor: &pbws.CloseCursorResp{}}})
	default:
		wc.replyErr(id, protoErr("unknown request"))
	}
}

func (wc *wsConn) send(m *pbws.ServerMsg) {
	var (
		data []byte
		typ  = websocket.MessageText
		err  error
	)
	if wc.protobuf {
		typ = websocket.MessageBinary
		data, err = proto.Marshal(m)
	} else {
		data, err = json.Marshal(jsonServerMsg(m))
	}
	if err != nil {
		return
	}
	wc.wmu.Lock()
	defer wc.wmu.Unlock()
	wc.c.Write(wc.ctx, typ, data)
}

func (wc *wsConn) decode(data []byte) (*pbws.ClientMsg, error) {
	if wc.protobuf {
		m := &pbws.ClientMsg{}
		if err := proto.Unmarshal(data, m); err != nil {
			return nil, err
		}
		return m, nil
	}
	var j jClientMsg
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, err
	}
	return j.proto()
}
