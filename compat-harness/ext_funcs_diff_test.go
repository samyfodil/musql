package compat

// Tests extended functions including timediff, rtree functions, fts3
// functions, and authentication functions.

import (
	"testing"

	_ "github.com/mattn/go-sqlite3"
	_ "github.com/samyfodil/musql/driver"
)

func TestExtFunctionsMatchOracle(t *testing.T) {
	for _, q := range []string{
		`SELECT sqlite_source_id(), typeof(sqlite_log(1,'x')), sqlite_log(NULL, 5)`,
		`SELECT load_extension('x')`,
		`SELECT load_extension('x', 'y')`,
		`SELECT authenticate('a','b'), auth_enabled(), auth_user_add('a',x'00',1), auth_user_change('a','b',-5), auth_user_delete(x'')`,
		`SELECT authenticate(NULL,'b')`,
		`SELECT auth_user_add('a','b','1')`,
		`SELECT rtreedepth(x'0001'), rtreedepth(x'ffff00'), rtreedepth(x'0203')`,
		`SELECT rtreedepth('ab')`,
		`SELECT rtreenode(1, x'00000001000000000000000a3fc0000040200000')`,
		`SELECT rtreenode(2, x'00000002000000000000000100000000000000004120000041a00000000000000000000000000000000000000000000000000000000000000000000000000000004120000041a00000')`,
		`SELECT rtreenode(1, x'0000000100000000000000077f8000007fc00000'), rtreenode(1, x'000000010000000000000007ff800000497423f0')`,
		`SELECT rtreenode(0, x'00000001000000000000000a3fc0000040200000'), rtreenode(257, x'00000001000000000000000a3fc0000040200000'), rtreenode('1e5', x'00000001000000000000000a3fc0000040200000')`,
		`SELECT rtreenode(1, x'00000000'), rtreenode(1, x''), rtreenode(1, NULL), rtreenode(1, x'000000010000')`,
		`SELECT rtreecheck()`,
		`SELECT fts3_tokenizer('simple') IS NULL, fts3_tokenizer('porter') IS NULL, fts3_tokenizer('unicode61') IS NULL`,
		`SELECT fts3_tokenizer('icu')`,
		`SELECT fts3_tokenizer('simple', x'0000000000000000')`,
		`SELECT count(*) FROM pragma_function_list WHERE name='fts3_tokenizer'`,
	} {
		differ(t, "extfuncs", []string{q})
	}
}

func TestTimediffMatchesOracle(t *testing.T) {
	for _, q := range []string{
		`SELECT timediff('2024-03-01','2024-02-01'), timediff('2024-02-01','2024-03-01'), timediff('2023-01-01 12:34:56.789','2024-02-29 01:02:03.456')`,
		`SELECT timediff('2024-01-31','2024-02-29'), timediff('2024-02-29','2024-01-31'), timediff('2000-01-01','1999-12-31 23:59:59.999')`,
		`SELECT timediff(2460000.5, 2450000.25), timediff(NULL, '2024-01-01'), timediff('2024-01-01', 'bogus')`,
		`SELECT timediff('-4713-11-24 12:00:00', '9999-12-31 23:59:59'), timediff('9999-12-31 23:59:59', '-4713-11-24 12:00:00')`,
		`SELECT timediff('2024-03-31', '2024-02-29'), timediff('2024-05-31','2024-04-30'), timediff('2023-03-01','2024-02-29')`,
		`SELECT timediff('0000-01-01', '-0001-12-31'), timediff('12:00', '2000-01-01 00:00')`,
		`SELECT timediff(-1, 0), timediff(5373484.5, 0), timediff(0, 5373484.49999)`,
		`SELECT timediff('2024-01-01T00:00:00+05:00', '2024-01-01'), timediff('2024-01-01 00:00:00Z', '2024-01-01 00:00:00-01:30')`,
		`SELECT timediff(' 2024-01-01', '2024-01-01'), timediff('2024-01-01 24:00:00', '2024-01-01'), timediff('2024-02-30', '2024-02-01')`,
		`SELECT timediff('2024-01-01 12:00:00 +14:59', '2024-01-01'), timediff('2024-01-01 12:00:00 +15:00', '2024-01-01'), timediff('12:00:00.5-01:00', '12:00')`,
		`SELECT timediff('14712-01-01', '2024-01-01'), timediff('2024-13-01', '2024-01-01'), timediff(x'323032342d30312d3031', '2023-01-01'), timediff('2460000.5', '2024-01-01')`,
		`SELECT timediff('2024-01-01')`,
	} {
		differ(t, "timediff", []string{q})
	}
}

// TestSchemaUseOfExtFunctions pins SQLITE_DIRECTONLY (refused from a main-schema
// view whatever trusted_schema says, allowed from a TEMP one) and the
// non-innocuous functions trusted_schema=OFF refuses -- including after the
// same statement already ran under the other setting on a warm pager.
func TestSchemaUseOfExtFunctions(t *testing.T) {
	differ(t, "schemause", []string{
		`CREATE TABLE tl(y)`,
		`INSERT INTO tl VALUES(5)`,
		`CREATE VIEW vfx AS SELECT fts3_tokenizer('simple') AS t`,
		`SELECT * FROM vfx`,
		`CREATE TEMP VIEW vft AS SELECT fts3_tokenizer('simple') AS t`,
		`SELECT * FROM vft`,
		`CREATE VIEW vfz AS SELECT rtreedepth(x'0001') AS d, authenticate('a','b') AS a FROM tl`,
		`SELECT * FROM vfz`,
		`PRAGMA trusted_schema=OFF`,
		`SELECT * FROM vfz`,
		`SELECT * FROM vft`,
		`PRAGMA trusted_schema=ON`,
		`SELECT * FROM vfz`,
		`CREATE TABLE tfc(a, CHECK(fts3_tokenizer('simple') IS NULL))`,
		`CREATE INDEX tl_i1 ON tl(sqlite_source_id())`,
		`CREATE INDEX tl_i2 ON tl(rtreedepth(y))`,
		`CREATE TABLE tfy(a, b AS (sqlite_source_id()))`,
	})
}

func TestPrintfInfinity(t *testing.T) {
	for _, q := range []string{
		`SELECT printf('%g|%e|%f|%10g|%-10g|%+g', 9e999, -9e999, 9e999, 9e999, -9e999, 9e999)`,
		`SELECT printf('%010g', 9e999), printf('%010g', -9e999), printf('%+012e', 9e999), printf('%#012g', 9e999), printf('%!012g', 9e999), printf('%015.2e', -9e999), printf('%0E', 9e999), printf('%0G', 9e999)`,
		`SELECT length(printf('%010f', 9e999)), substr(printf('%010f', 9e999), -10), length(printf('%!0.0f', 9e999)), substr(printf('%!0.0f', 9e999), -5)`,
		`SELECT printf('%g', '1e999'), printf('%5.1f', -9e999)`,
	} {
		differ(t, "printfinf", []string{q})
	}
}
