package hrana

import (
	"bufio"
	"bytes"
	"database/sql/driver"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	pb "github.com/samyfodil/musql/hrana/gen/hrana"
	pbhttp "github.com/samyfodil/musql/hrana/gen/hrana/http"
)

func postProtobuf(t *testing.T, url string, m interface{ MarshalVT() ([]byte, error) }) []byte {
	t.Helper()
	body, err := m.MarshalVT()
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url, "application/x-protobuf", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("%s: %d %v %q", url, resp.StatusCode, err, b)
	}
	return b
}

// TestProtobufTextNotUTF8: a TEXT value that is not UTF-8 goes out as the
// bytes the database holds, on the pipeline and in a cursor, rather than
// failing the whole response or leaving its row out of the cursor.
func TestProtobufTextNotUTF8(t *testing.T) {
	url := newTestServer(t)
	q := "SELECT CAST(x'ff61' AS TEXT), 7"
	stmt := &pb.Stmt{Sql: &q}

	var out pbhttp.PipelineRespBody
	if err := out.UnmarshalVT(postProtobuf(t, url+"/v3-protobuf/pipeline", &pbhttp.PipelineReqBody{Requests: []*pbhttp.StreamRequest{
		{Request: &pbhttp.StreamRequest_Execute{Execute: &pbhttp.ExecuteStreamReq{Stmt: stmt}}}}})); err != nil {
		t.Fatal(err)
	}
	rows := out.GetResults()[0].GetOk().GetExecute().GetResult().GetRows()
	if len(rows) != 1 || rows[0].Values[0].GetText() != "\xffa" || rows[0].Values[1].GetInteger() != 7 {
		t.Fatalf("pipeline rows: %v", rows)
	}

	r := bufio.NewReader(bytes.NewReader(postProtobuf(t, url+"/v3-protobuf/cursor", &pbhttp.CursorReqBody{Baton: out.Baton, Batch: &pb.Batch{Steps: []*pb.BatchStep{{Stmt: stmt}}}})))
	next := func(m interface{ UnmarshalVT([]byte) error }) bool {
		n, err := binary.ReadUvarint(r)
		if err == io.EOF {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			t.Fatal(err)
		}
		if err := m.UnmarshalVT(b); err != nil {
			t.Fatal(err)
		}
		return true
	}
	if !next(&pbhttp.CursorRespBody{}) {
		t.Fatal("no cursor head")
	}
	var got []string
	for {
		var e pb.CursorEntry
		if !next(&e) {
			break
		}
		switch x := e.Entry.(type) {
		case *pb.CursorEntry_StepBegin:
			got = append(got, "begin")
		case *pb.CursorEntry_Row:
			got = append(got, "row "+x.Row.Values[0].GetText())
		case *pb.CursorEntry_StepEnd:
			got = append(got, "end")
		default:
			got = append(got, "other")
		}
	}
	if len(got) != 3 || got[1] != "row \xffa" {
		t.Fatalf("cursor entries: %q", got)
	}
}

// TestRowAllocValues: rows built from slabs hold every scanned value, for
// each type a scan yields, across rows of every width and many chunks, and a
// BLOB's bytes are copied, since the driver's are its own after the next Scan.
func TestRowAllocValues(t *testing.T) {
	var a rowAlloc
	ts := time.Date(2026, 10, 30, 1, 2, 3, 0, time.UTC)
	full := func(i int) []driver.Value {
		return []driver.Value{int64(i), float64(i) + 0.5, fmt.Sprint(i), []byte{byte(i)}, nil, i%2 == 0, ts, uint8(7)}
	}
	var rows []*pb.Row
	for i := range 3000 {
		vals := full(i)[:1+i%8]
		rows = append(rows, a.row(vals))
		if len(vals) > 3 {
			vals[3].([]byte)[0] ^= 0xff
		}
	}
	for i, r := range rows {
		want := full(i)[:1+i%8]
		if len(r.Values) != len(want) {
			t.Fatalf("row %d: %d values, want %d", i, len(r.Values), len(want))
		}
		ok := true
		for j, v := range r.Values {
			switch j {
			case 0:
				ok = ok && v.GetInteger() == int64(i)
			case 1:
				ok = ok && v.GetFloat() == float64(i)+0.5
			case 2:
				ok = ok && v.GetText() == fmt.Sprint(i)
			case 3:
				ok = ok && bytes.Equal(v.GetBlob(), []byte{byte(i)})
			case 4:
				ok = ok && v.GetNull() != nil
			case 5:
				ok = ok && v.GetInteger() == int64(1-i%2)
			case 6:
				ok = ok && v.GetText() == "2026-10-30 01:02:03+00:00"
			case 7:
				ok = ok && v.GetText() == "7"
			}
		}
		if !ok {
			t.Fatalf("row %d: %v, want %v", i, r.Values, want)
		}
	}
}

// TestNamedArgs: named arguments bind by name whatever their prefix, and one
// whose name does not begin with a letter fails the statement with the text
// database/sql gave it.
func TestNamedArgs(t *testing.T) {
	url := newTestServer(t)
	q := "SELECT :a + 1, @b, $c"
	named := func(names ...string) *pbhttp.PipelineReqBody {
		vals := []*pb.Value{{Value: &pb.Value_Integer{Integer: 41}}, {Value: &pb.Value_Text{Text: "x"}}, {Value: &pb.Value_Blob{Blob: []byte{1, 2}}}}
		st := &pb.Stmt{Sql: &q}
		for i, n := range names {
			st.NamedArgs = append(st.NamedArgs, &pb.NamedArg{Name: n, Value: vals[i]})
		}
		return &pbhttp.PipelineReqBody{Requests: []*pbhttp.StreamRequest{
			{Request: &pbhttp.StreamRequest_Execute{Execute: &pbhttp.ExecuteStreamReq{Stmt: st}}}}}
	}
	var out pbhttp.PipelineRespBody
	if err := out.UnmarshalVT(postProtobuf(t, url+"/v3-protobuf/pipeline", named(":a", "@b", "$c"))); err != nil {
		t.Fatal(err)
	}
	rows := out.GetResults()[0].GetOk().GetExecute().GetResult().GetRows()
	if len(rows) != 1 || rows[0].Values[0].GetInteger() != 42 || rows[0].Values[1].GetText() != "x" || !bytes.Equal(rows[0].Values[2].GetBlob(), []byte{1, 2}) {
		t.Fatalf("named args: %v %v", rows, out.GetResults()[0].GetError())
	}
	out.Reset()
	if err := out.UnmarshalVT(postProtobuf(t, url+"/v3-protobuf/pipeline", named(":a", "@b", "$1"))); err != nil {
		t.Fatal(err)
	}
	if e := out.GetResults()[0].GetError(); e.GetMessage() != `name "1" does not begin with a letter` {
		t.Fatalf("a name not beginning with a letter: %v", e)
	}
}
