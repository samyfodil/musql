package compat

import "testing"

// TestFts3CorruptPoslistByteRules verifies position list byte decoding
// handles non-minimal varints and column list terminators correctly.
func TestFts3CorruptPoslistByteRules(t *testing.T) {
	differ(t, "fts3 corrupt position list, byte rules", []string{
		"CREATE VIRTUAL TABLE t USING fts4(x)",
		"INSERT INTO t_content(docid,c0x) VALUES(1,'a b')",
		"DELETE FROM t_segments",
		"DELETE FROM t_segdir",
		"INSERT INTO t_segdir(level,idx,start_block,leaves_end_block,end_block,root) VALUES(0,0,0,0,0,x'000161110150028001500a818080800103038101000001622c010281008101ffffffffffffffffff010204ffffffffffffffffff01ffffffffffffffffff01c80109323200')",
		"SELECT count(*) FROM t WHERE t MATCH 'a NEAR/100 b'",
		"SELECT count(*) FROM t WHERE t MATCH 'a'",
		"SELECT count(*) FROM t WHERE t MATCH 'b'",
		"SELECT count(*) FROM t WHERE t MATCH 'a b'",
		"SELECT count(*) FROM t WHERE t MATCH 'a NEAR b'",
	})
}
