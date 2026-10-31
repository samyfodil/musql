package hrana

import (
	"bufio"
	"bytes"
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
// each type a scan yields, across rows of every width and many chunks.
func TestRowAllocValues(t *testing.T) {
	var a rowAlloc
	ts := time.Date(2026, 10, 30, 1, 2, 3, 0, time.UTC)
	full := func(i int) []any {
		return []any{int64(i), float64(i) + 0.5, fmt.Sprint(i), []byte{byte(i)}, nil, i%2 == 0, ts, uint8(7)}
	}
	var rows []*pb.Row
	for i := range 3000 {
		rows = append(rows, a.row(full(i)[:1+i%8]))
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
