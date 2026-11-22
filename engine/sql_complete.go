package engine

import "strings"

// Complete reports whether sql ends with a complete statement: a ";" outside
// any literal, comment or trigger body. It is C's sqlite3_complete, which a
// shell uses to decide whether to read another line, so it tokenizes only as
// far as that needs and accepts text the parser would reject.
func Complete(sql string) bool {
	const (
		tkSemi = iota
		tkWS
		tkOther
		tkExplain
		tkCreate
		tkTemp
		tkTrigger
		tkEnd
	)
	// States: 0 invalid, 1 start, 2 normal, 3 explain, 4 create, 5 trigger,
	// 6 semicolon in a trigger body, 7 END in a trigger body.
	trans := [8][8]uint8{
		{1, 0, 2, 3, 4, 2, 2, 2},
		{1, 1, 2, 3, 4, 2, 2, 2},
		{1, 2, 2, 2, 2, 2, 2, 2},
		{1, 3, 3, 2, 4, 2, 2, 2},
		{1, 4, 2, 2, 2, 4, 5, 2},
		{6, 5, 5, 5, 5, 5, 5, 5},
		{6, 6, 5, 5, 5, 5, 5, 7},
		{1, 7, 5, 5, 5, 5, 5, 5},
	}
	var state uint8
	n := len(sql)
	for i := 0; i < n; i++ {
		var tok int
		switch c := sql[i]; c {
		case ';':
			tok = tkSemi
		case ' ', '\r', '\t', '\n', '\f':
			tok = tkWS
		case '/':
			if i+1 >= n || sql[i+1] != '*' {
				tok = tkOther
				break
			}
			end := strings.Index(sql[i+2:], "*/")
			if end < 0 {
				return false
			}
			i += 2 + end + 1
			tok = tkWS
		case '-':
			if i+1 >= n || sql[i+1] != '-' {
				tok = tkOther
				break
			}
			nl := strings.IndexByte(sql[i:], '\n')
			if nl < 0 {
				return state == 1
			}
			i += nl
			tok = tkWS
		case '[', '`', '"', '\'':
			closer := c
			if c == '[' {
				closer = ']'
			}
			end := strings.IndexByte(sql[i+1:], closer)
			if end < 0 {
				return false
			}
			i += 1 + end
			tok = tkOther
		default:
			if !isIdentContinueByte(c) {
				tok = tkOther
				break
			}
			j := i + 1
			for j < n && isIdentContinueByte(sql[j]) {
				j++
			}
			switch word := sql[i:j]; {
			case strings.EqualFold(word, "create"):
				tok = tkCreate
			case strings.EqualFold(word, "trigger"):
				tok = tkTrigger
			case strings.EqualFold(word, "temp"), strings.EqualFold(word, "temporary"):
				tok = tkTemp
			case strings.EqualFold(word, "end"):
				tok = tkEnd
			case strings.EqualFold(word, "explain"):
				tok = tkExplain
			default:
				tok = tkOther
			}
			i = j - 1
		}
		state = trans[state][tok]
	}
	return state == 1
}
