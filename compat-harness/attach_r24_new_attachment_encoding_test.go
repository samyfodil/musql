// This file tests that attached databases inherit the connection's text encoding.
// New databases created by ATTACH get the connection's encoding, and existing
// files in a different encoding are rejected.
package compat

import (
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestR24NewAttachmentInheritsConnectionEncoding(t *testing.T) {
	for _, enc := range []string{"utf8", "utf16", "utf16le", "utf16be"} {
		enc := enc
		t.Run(enc, func(t *testing.T) {
			dir := t.TempDir()
			p := newAttachPair(t,
				[]string{filepath.Join(dir, "go-new.db2")},
				[]string{filepath.Join(dir, "cgo-new.db2")})
			p.agreeExec("PRAGMA encoding = '" + enc + "'")
			p.agreeExec("CREATE TABLE m(x)")
			p.agreeExec("ATTACH '{0}' AS aux")
			// The half that actually matters: text WRITTEN through the
			// attachment comes back with the same BYTES on both engines, which
			// is only true if the file this ATTACH created carries the
			// connection's encoding in its header. (A qualified
			// "PRAGMA aux.encoding" is not used as the probe: that shape is a
			// separate, pre-existing gap on the snapshot-pager read path --
			// "unknown database aux" -- and would gate the wrong thing.)
			p.agreeExec("CREATE TABLE aux.t2(x)")
			p.agreeExec("INSERT INTO aux.t2 VALUES('héllo')")
			p.agreeQuery("SELECT x, hex(x), length(x) FROM aux.t2")
			p.agreeQuery("SELECT hex(x) FROM aux.t2 UNION ALL SELECT hex('héllo')")
			p.agreeExec("DETACH aux")
		})
	}
}

// TestR24AttachOfAnExistingFileStillMustAgreeOnEncoding tests that existing files in a different encoding are rejected.
func TestR24AttachOfAnExistingFileStillMustAgreeOnEncoding(t *testing.T) {
	goAux, cgoAux := buildAuxPair(t, "u16",
		"PRAGMA encoding = 'utf16'",
		"CREATE TABLE t2(x)",
		"INSERT INTO t2 VALUES('text2')")
	p := newAttachPair(t, []string{goAux}, []string{cgoAux})
	p.agreeExec("CREATE TABLE m(x)") // main is UTF-8: the encoding is now fixed
	p.declineExec("ATTACH '{0}' AS aux")

	// ...and the same file attached from a connection that IS utf16 is fine,
	// which is attach2.test 2.1's real point.
	q := newAttachPair(t, []string{goAux}, []string{cgoAux})
	q.agreeExec("PRAGMA encoding = 'utf16'")
	q.agreeExec("ATTACH '{0}' AS aux")
	q.agreeQuery("SELECT x, hex(x) FROM aux.t2")
}
