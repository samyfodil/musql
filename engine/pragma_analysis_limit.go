package engine

// PRAGMA analysis_limit, and the SAMPLED ANALYZE it selects.
//
// The pragma itself is trivial (pragma.c:2727-2736): both forms answer one row
// of one INTEGER column named analysis_limit, and the setter stores the value
// only when sqlite3DecOrHexToI64 parses the WHOLE payload and the result is
// >= 0 -- anything else leaves the limit exactly as it was and still reports
// it. It was declined here, getter included, because the limit changes what
// ANALYZE writes and accepting it without that would be the
// accept-and-ignore wrong answer.
//
// So this file also carries the other half: analyze.c's limited scan. With
// db->nAnalysisLimit set, ANALYZE stops walking an index early, and the
// stopping rule is not "look at N rows" -- it is a SKIP-AHEAD, spelled out by
// statPush (analyze.c:1310-1313) and the codegen around its call
// (analyze.c:1249-1263):
//
//	statPush: if( p->nLimit && p->nRow > p->nLimit*(p->nSkipAhead+1) ){
//	            p->nSkipAhead++;
//	            sqlite3_result_int(context, p->current.anDLt[0]>0);
//	          }
//
//	codegen: NULL result   -> OP_Next, keep scanning
//	         true result   -> fall past the Next: STOP this index
//	         false result  -> OP_SeekGT past the current leading value, and
//	                          keep scanning from there
//
// In words: scan until row count passes the limit; if the first key column
// has changed, stop; otherwise jump to the next distinct first-column value
// and carry on with a raised budget. ANALYZE still reports the table's true
// row count first and sampled figures after.

// pragmaAnalysisLimit answers PRAGMA [schema.]analysis_limit. The qualifier is
// ignored: the limit is connection state, not per-database.
func pragmaAnalysisLimit(stmt *PragmaStmt, st *PragmaConnState) (cols []string, rows [][]Value, handled bool, err error) {
	if stmt.HasValue {
		if serr := pragmaValueSpellingSupported(stmt); serr != nil {
			return nil, nil, true, serr
		}
		// "sqlite3DecOrHexToI64(zRight,&N)==SQLITE_OK && N>=0" -- a payload
		// that does not parse cleanly, or a negative one, leaves the limit
		// alone and is NOT an error.
		if n, ok := pragmaDecOrHexToI64OK(stmt.ValueText); ok && n >= 0 {
			st.AnalysisLimit = int(n & 0x7fffffff)
		}
	}
	// returnSingleInt sits OUTSIDE the if: the setter reports the resulting
	// limit too, unlike every PragTyp_FLAG setter's empty result set.
	return []string{"analysis_limit"}, [][]Value{{{Typ: Int, I: int64(st.AnalysisLimit)}}}, true, nil
}

// pragmaDecOrHexToI64OK is sqlite3DecOrHexToI64 (util.c) WITH its status: the
// value is usable only when the whole payload is a number. pragmaDecOrHexToI64
// above answers the same value and swallows the status, which every other
// caller wants (they have their own clamp); analysis_limit is the one caller
// that must distinguish "0" from "not a number".
func pragmaDecOrHexToI64OK(z string) (int64, bool) {
	if len(z) >= 2 && z[0] == '0' && (z[1] == 'x' || z[1] == 'X') {
		i := 2
		for i < len(z) && z[i] == '0' {
			i++
		}
		k := i
		var u uint64
		for k < len(z) && isHexDigitByte(z[k]) {
			u = u*16 + uint64(hexDigitValue(z[k]))
			k++
		}
		if k-i > 16 || k < len(z) {
			return int64(u), false // rc 2 (too wide) or rc 1 (trailing junk)
		}
		return int64(u), true
	}
	// sqlite3Atoi64's own success test: optional leading space, an optional
	// sign, at least one digit, and nothing after it but space. Any other
	// byte anywhere makes strspn stop short, which puts a non-digit inside
	// Atoi64's window and gives rc 1.
	i := 0
	for i < len(z) && isSpaceByte(z[i]) {
		i++
	}
	neg := false
	if i < len(z) && (z[i] == '-' || z[i] == '+') {
		neg = z[i] == '-'
		i++
	}
	start := i
	for i < len(z) && z[i] == '0' {
		i++
	}
	var u uint64
	digits := 0
	for ; i < len(z) && z[i] >= '0' && z[i] <= '9'; i++ {
		digits++
		if digits > 19 {
			return 0, false
		}
		u = u*10 + uint64(z[i]-'0')
	}
	if i == start {
		return 0, false // no digits at all
	}
	for ; i < len(z); i++ {
		if !isSpaceByte(z[i]) {
			return 0, false // extra non-space text after the integer
		}
	}
	if u > 1<<63-1 {
		return 0, false
	}
	if neg {
		return -int64(u), true
	}
	return int64(u), true
}

// analysisLimitWalk is analyze.c's index scan under a limit. recs must already
// be sorted by the index's own collation. It returns the number of rows the
// scan visited, how many times it SKIPPED AHEAD (statGet reports the index's
// true row count instead of nRow whenever that is nonzero), and, per
// key-column prefix, how many distinct prefixes it saw minus one -- statPush's
// anDLt, which statGet turns into the rest of the stat1 string.
//
// With limit 0 it is a plain full pass, which is what every ANALYZE without
// the pragma gets.
func analysisLimitWalk(
	recs [][]Value,
	nKey int,
	limit int,
	equalPrefix func(a, b []Value, n int) bool,
) (nRow, skipAhead int, anDLt []int) {
	anDLt = make([]int, nKey)
	prev := -1 // the last row PUSHED, which is what regPrev holds
	for i := 0; i < len(recs); {
		if prev >= 0 {
			// iChng, the first key column at which this record differs from
			// the last pushed one. statPush bumps anDLt for that column and
			// every one after it.
			chng := nKey
			for c := 0; c < nKey; c++ {
				if !equalPrefix(recs[prev], recs[i], c+1) {
					chng = c
					break
				}
			}
			for c := chng; c < nKey; c++ {
				anDLt[c]++
			}
		}
		prev = i
		nRow++
		if limit > 0 && nRow > limit*(skipAhead+1) {
			// nSkipAhead is bumped even on the arm that stops, which is what
			// makes statGet report the true count rather than nRow.
			skipAhead++
			if anDLt[0] > 0 {
				break // the leading column already changed: stop here
			}
			// OP_SeekGT past every remaining row with this leading value.
			j := i + 1
			for j < len(recs) && equalPrefix(recs[i], recs[j], 1) {
				j++
			}
			if j >= len(recs) {
				break // SeekGT found nothing: it jumps to the end
			}
			// A SUCCESSFUL SeekGT falls through into the same OP_Next the
			// no-limit path uses (analyze.c:1257, the JumpHere(j1) target),
			// so the row the seek landed on is itself stepped over. Off by
			// this one row, a two-column index's second figure comes out
			// wrong: measured at analysis_limit=715 over (b,a), the oracle
			// says "5000 716 179" and counting the landed-on row says 205.
			if j+1 >= len(recs) {
				break
			}
			i = j + 1
			continue
		}
		i++
	}
	return nRow, skipAhead, anDLt
}

// analysisLimit is this session's "PRAGMA analysis_limit", 0 (no limit) when
// it was never set. It lives in PragmaConnState because the value is
// CONNECTION state and this engine's DB is per-statement in the driver -- the
// same reason every other tuning value does.
func (db *DB) analysisLimit() int {
	if db.pragmaState == nil {
		return 0
	}
	return db.pragmaState.AnalysisLimit
}

// SetAnalysisLimit carries "PRAGMA analysis_limit" onto a write session, for
// the driver, whose Conn owns the connection state an ANALYZE's throwaway
// session would otherwise never see.
func (db *DB) SetAnalysisLimit(n int) {
	if db.pragmaState == nil {
		db.pragmaState = &PragmaConnState{}
	}
	db.pragmaState.AnalysisLimit = n
}

// AnalysisLimitValue reports the limit a connection's state holds, 0 for a nil
// state -- the driver's read-back half, which pairs with SetAnalysisLimit.
func (st *PragmaConnState) AnalysisLimitValue() int {
	if st == nil {
		return 0
	}
	return st.AnalysisLimit
}
