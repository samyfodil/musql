package compat

// Differential integrity checking: runs C SQLite's integrity_check on files
// written during corpus replay to catch corruption not visible in answer comparison.
// Findings are reported, not failed.

import (
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/samyfodil/musql/engine"
)

// tclXIntTally is one run's census, summed over every segment.
type tclXIntTally struct {
	checks     int           // integrity_checks asked (musql's files and, when needed, the oracle's)
	bad        int           // findings logged: a site's first not-ok musql file whose oracle copy was ok
	badSegs    int           // segments with at least one finding
	bothBad    int           // musql not ok and the oracle's own copy not ok either: intended corruption
	unreadable int           // cgo could not answer for the musql file (locked, ...) nor for the oracle's
	pdPanics   int           // driver panicked mid-replay; the rest of that segment was not replayed
	pdTime     time.Duration // spent replaying through driver
	checkTime  time.Duration // spent in the cgo integrity_checks
	closeTime  time.Duration // spent Closing the engine-direct sessions
	buckets    map[string]int
}

var tclXInt = tclXIntTally{buckets: map[string]int{}}

func tclCrossIntegrityOn() bool { return os.Getenv("TCL_CROSS_INTEGRITY") != "" }

type tclCrossIntegrity struct {
	t                 *testing.T
	label             string
	pd                *sql.DB
	pdPath            string
	pdAttachDir       string
	cgoPath           string
	cgoAttachDir      string
	recent            []string
	dead              bool
	diverged          bool
	flagged, flaggedE bool
	anyFinding        bool
	bothNoted         bool
}

// newTCLCrossIntegrity returns nil when TCL_CROSS_INTEGRITY is unset; every
// method is a no-op on nil, so runTCLSegment's hooks cost nothing then.
func newTCLCrossIntegrity(t *testing.T, label, cgoPath, cgoAttachDir string) *tclCrossIntegrity {
	if !tclCrossIntegrityOn() {
		return nil
	}
	x := &tclCrossIntegrity{
		t:            t,
		label:        label,
		pdPath:       filepath.Join(t.TempDir(), "pd.db"),
		pdAttachDir:  t.TempDir(),
		cgoPath:      cgoPath,
		cgoAttachDir: cgoAttachDir,
	}
	pd, err := sql.Open("sqlite", x.pdPath)
	if err != nil {
		t.Fatalf("sql.Open(sqlite): %v", err)
	}
	// One connection, for the same reason cgodb has one: a BEGIN must reach
	// the statements after it.
	pd.SetMaxOpenConns(1)
	x.pd = pd
	return x
}

// exec replays one statement the oracle ran for real and then checks the file.
// oracleOK is whether the oracle accepted it.
func (x *tclCrossIntegrity) exec(i int, stmt string, oracleOK bool) {
	x.run(i, stmt, oracleOK, func(s string) error {
		_, err := x.pd.Exec(s)
		return err
	})
}

// query is exec for a SELECT the oracle ran that can write (tclQueryMutatesOracle).
func (x *tclCrossIntegrity) query(i int, stmt string, oracleOK bool) {
	x.run(i, stmt, oracleOK, func(s string) error {
		rows, err := x.pd.Query(s)
		if err != nil {
			return err
		}
		for rows.Next() {
		}
		return rows.Close()
	})
}

func (x *tclCrossIntegrity) run(i int, stmt string, oracleOK bool, do func(string) error) {
	if x == nil || x.dead {
		return
	}
	one := tclXIntOneLine(stmt)
	x.recent = append(x.recent, one)
	if len(x.recent) > 4 {
		x.recent = x.recent[1:]
	}
	start := time.Now()
	func() {
		defer func() {
			if r := recover(); r != nil {
				// database/sql's connection state is unknowable after a driver
				// panic, so stop replaying rather than check a half-run session.
				x.dead = true
				tclXInt.pdPanics++
				x.t.Logf("XINTEGRITY-PDPANIC\t%s\t%d\t%s\t%v", x.label, i, one, r)
			}
		}()
		if err := do(tclIsolateAttach(stmt, x.pdAttachDir)); err != nil && oracleOK && !x.diverged {
			x.diverged = true
			x.t.Logf("XINTEGRITY-DIVERGED\t%s\t%d\t%s\t%v", x.label, i, one, err)
		}
	}()
	tclXInt.pdTime += time.Since(start)
	if x.dead {
		return
	}
	if fi, err := os.Stat(x.pdPath); err != nil || fi.Size() == 0 {
		return
	}
	msg, oracleMsg, bad := tclXIntJudge(x.pdPath, x.cgoPath)
	if !bad && oracleMsg != "" && !x.bothNoted && tclXIntBucket(msg) != tclXIntBucket(oracleMsg) {
		// Both files not ok, for DIFFERENT reasons: not counted as a finding,
		// since the oracle's copy is broken too, but a writer bug can hide
		// behind an intended corruption, so it is left to read.
		x.bothNoted = true
		x.t.Logf("XINTEGRITY-BOTH\t%s\t%d\t%s\tmusql: %s\toracle: %s", x.label, i, tclXIntOneLine(stmt), tclXIntBucket(msg), tclXIntBucket(oracleMsg))
	}
	if bad && !x.flagged {
		x.flagged = true
		site := "stmt"
		if x.diverged {
			site = "stmt-diverged"
		}
		x.report(site, i, msg, x.recent)
	}
}

// finish closes both sessions and checks what they left: the engine-direct
// session's own file and every database either musql session ATTACHed.
func (x *tclCrossIntegrity) finish(godb *engine.Session, goPath, segDir string) {
	if x == nil {
		return
	}
	x.pd.Close()
	if msg, bad := x.judgeAttached(x.pdAttachDir); bad && !x.flagged {
		x.report("attach-stmt", -1, msg, x.recent)
	}
	start := time.Now()
	closeErr := func() (err error) {
		// This Close is the harness's own addition, and C SQLite's
		// integrity_check below is the judgement asked of it.
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("Close panicked: %v", r)
			}
		}()
		return godb.Close()
	}()
	tclXInt.closeTime += time.Since(start)
	if closeErr != nil {
		// Not a file finding -- nothing was written to judge -- but a Close
		// that errors or panics after a segment the corpus called clean is
		// worth reading, so it is logged under its own tag.
		x.t.Logf("XINTEGRITY-CLOSEERR\t%s\t%v", x.label, closeErr)
		return
	}
	if msg, _, bad := tclXIntJudge(goPath, x.cgoPath); bad {
		x.flaggedE = true
		x.report("end", -1, msg, nil)
	}
	if msg, bad := x.judgeAttached(segDir); bad && !x.flaggedE {
		x.report("attach-end", -1, msg, nil)
	}
}

// judgeAttached checks every SQLite file under dir against the oracle's file
// at the same relative path (tclIsolateAttach moves the oracle's ATTACH targets
// by the same relative path).
func (x *tclCrossIntegrity) judgeAttached(dir string) (msg string, bad bool) {
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || bad || !tclXIntIsDatabase(p) {
			return nil
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return nil
		}
		msg, _, bad = tclXIntJudge(p, filepath.Join(x.cgoAttachDir, rel))
		if bad {
			msg = rel + ": " + msg
		}
		return nil
	})
	return msg, bad
}

func (x *tclCrossIntegrity) report(site string, i int, msg string, recent []string) {
	tclXInt.bad++
	if !x.anyFinding {
		x.anyFinding = true
		tclXInt.badSegs++
	}
	bucket := site + " | " + tclXIntBucket(msg)
	tclXInt.buckets[bucket]++
	x.t.Logf("XINTEGRITY\t%s\t%s\t%d\t%s\t%s", site, x.label, i, strings.Join(recent, " ;; "), msg)
}

// tclXIntJudge asks C SQLite about the musql file at path and, only when
// that is not "ok", about the oracle's file at oraclePath. bad is true only
// when the oracle's own copy is fine; oracleMsg is the oracle's own answer
// when it is not.
func tclXIntJudge(path, oraclePath string) (msg, oracleMsg string, bad bool) {
	got, err := tclXIntCheck(path)
	if err == nil && got == "ok" {
		return "", "", false
	}
	if err != nil {
		got = "error: " + err.Error()
	}
	if _, serr := os.Stat(oraclePath); serr != nil {
		// No oracle counterpart (an ATTACH target only musql created): judge
		// the musql file alone.
		if err != nil {
			tclXInt.unreadable++
		}
		return got, "", err == nil
	}
	want, oerr := tclXIntCheck(oraclePath)
	if oerr != nil {
		want = "error: " + oerr.Error()
	}
	switch {
	case oerr == nil && want == "ok":
		return got, "", true
	case err != nil && oerr != nil:
		tclXInt.unreadable++
	default:
		tclXInt.bothBad++
	}
	return got, want, false
}

// tclXIntCheck runs PRAGMA integrity_check through a fresh read-only cgo
// connection, never waiting on a lock: a session holding one (locking_mode=
// EXCLUSIVE) is simply unreadable at that point.
func tclXIntCheck(path string) (string, error) {
	start := time.Now()
	defer func() {
		tclXInt.checks++
		tclXInt.checkTime += time.Since(start)
	}()
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&_busy_timeout=0"}
	db, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return "", err
	}
	defer db.Close()
	rows, err := db.Query("PRAGMA integrity_check")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var v sql.NullString
		if err := rows.Scan(&v); err != nil {
			return "", err
		}
		lines = append(lines, v.String)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return strings.Join(lines, "\n"), nil
}

// tclXIntIsDatabase reports whether p is a non-empty SQLite database file (a
// journal, WAL or wal-index never starts with the magic string).
func tclXIntIsDatabase(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [16]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return string(magic[:]) == "SQLite format 3\x00"
}

var tclXIntDigits = regexp.MustCompile(`[0-9]+`)

// tclXIntBucket keys a finding by its first line with every number blanked, so
// "page 2" and "page 7" land together.
func tclXIntBucket(msg string) string {
	msg = strings.TrimPrefix(msg, "*** in database main ***\n")
	first, _, _ := strings.Cut(msg, "\n")
	return tclXIntDigits.ReplaceAllString(strings.TrimSpace(first), "N")
}

func tclXIntOneLine(stmt string) string {
	one := strings.Join(strings.Fields(stmt), " ")
	if len(one) > 200 {
		one = one[:200] + "..."
	}
	return one
}

// tclXIntSummary logs the run's census and its buckets.
func tclXIntSummary(t *testing.T) {
	if !tclCrossIntegrityOn() {
		return
	}
	x := tclXInt
	// Not "TOTAL:" -- scripts/sweep takes a chunk's last TOTAL line as its verdict.
	t.Logf("XINTEGRITY SUMMARY checks=%d bad=%d badSegments=%d bothBad=%d unreadable=%d pdPanics=%d pdTime=%s checkTime=%s closeTime=%s",
		x.checks, x.bad, x.badSegs, x.bothBad, x.unreadable, x.pdPanics, x.pdTime.Round(time.Millisecond), x.checkTime.Round(time.Millisecond), x.closeTime.Round(time.Millisecond))
	for k, v := range x.buckets {
		t.Logf("XINTEGRITY BUCKET\t%d\t%s", v, k)
	}
}
