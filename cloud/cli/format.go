package cli

import (
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/samyfodil/musql/cloud/gen/cloud/v1"
)

// writeTable prints rows under headers, columns padded to their widest cell.
func writeTable(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) {
				widths[i] = max(widths[i], utf8.RuneCountInString(c))
			}
		}
	}
	line := func(cells []string) {
		var b strings.Builder
		for i, c := range cells {
			if i == len(cells)-1 {
				b.WriteString(c)
				break
			}
			b.WriteString(c)
			b.WriteString(strings.Repeat(" ", widths[i]-utf8.RuneCountInString(c)+2))
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
	line(headers)
	for _, r := range rows {
		line(r)
	}
}

func writeJSON(w io.Writer, m proto.Message) error {
	b, err := protojson.MarshalOptions{Multiline: true, Indent: "  ", EmitUnpopulated: true}.Marshal(m)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}

// list prints m as JSON with --json, else as a table.
func (a *app) list(m proto.Message, headers []string, rows [][]string) error {
	if a.json {
		return writeJSON(a.env.Stdout, m)
	}
	writeTable(a.env.Stdout, headers, rows)
	return nil
}

func when(t *timestamppb.Timestamp) string {
	if t == nil {
		return "-"
	}
	return t.AsTime().Local().Format(time.DateTime)
}

func roleName(r v1.Role) string {
	return strings.ToLower(strings.TrimPrefix(r.String(), "ROLE_"))
}

func parseRole(s string) (v1.Role, error) {
	r, ok := v1.Role_value["ROLE_"+strings.ToUpper(s)]
	if !ok || r == 0 {
		return 0, fmt.Errorf("unknown role %q (viewer, member, admin or owner)", s)
	}
	return v1.Role(r), nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// parseExpiration reads turso-style token lifetimes: "never", a number of
// days ("7d") or a Go duration ("12h").
func parseExpiration(s string, now time.Time) (*timestamppb.Timestamp, error) {
	if s == "" || s == "never" {
		return nil, nil
	}
	if d, ok := strings.CutSuffix(s, "d"); ok {
		var n int
		if _, err := fmt.Sscanf(d, "%d", &n); err != nil || n <= 0 || fmt.Sprint(n) != d {
			return nil, fmt.Errorf("bad expiration %q", s)
		}
		return timestamppb.New(now.AddDate(0, 0, n)), nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil || dur <= 0 {
		return nil, fmt.Errorf("bad expiration %q: use never, 7d or a duration like 12h", s)
	}
	return timestamppb.New(now.Add(dur)), nil
}

// parseTime reads an RFC 3339 time, or a date.
func parseTime(s string) (*timestamppb.Timestamp, error) {
	if s == "" {
		return nil, nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.DateTime, time.DateOnly} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return timestamppb.New(t), nil
		}
	}
	return nil, fmt.Errorf("bad timestamp %q: use RFC 3339, e.g. 2026-10-23T12:00:00Z", s)
}
