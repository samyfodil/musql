// The data and the queries both engines run. Each query's params come from a
// seeded generator, so both engines see the same values in the same order.
export const N = Number(new URLSearchParams(location.search).get("rows") || 100000);

export const setup = [
	"CREATE TABLE t(id INTEGER PRIMARY KEY, k INTEGER NOT NULL, v INTEGER NOT NULL, bid INTEGER NOT NULL, sec INTEGER NOT NULL, payload TEXT)",
	"CREATE TABLE b(id INTEGER PRIMARY KEY, name TEXT NOT NULL)",
	`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < ${N})
	 INSERT INTO t SELECT i, i % 10, (i * 7919) % 1000000, 1 + i % 1000, (i * 31) % ${N},
	 CASE WHEN i % 13 = 0 THEN NULL ELSE 'row-' || (i % 97) || '-payload' END FROM c`,
	`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < 1000)
	 INSERT INTO b SELECT i, 'b' || i FROM c`,
	"CREATE INDEX t_sec ON t(sec)",
	"CREATE INDEX t_bid ON t(bid)",
];

const rnd = (n) => (r) => Math.floor(r() * n);
export const workloads = [
	["Filtered count", "SELECT count(*) FROM t WHERE v > ?", (r) => [rnd(1000000)(r)]],
	["Filtered count, two predicates", "SELECT count(*) FROM t WHERE v > ? AND k <> ?", (r) => [rnd(1000000)(r), rnd(10)(r)]],
	["Sum over a filter", "SELECT sum(v) FROM t WHERE v < ?", (r) => [rnd(1000000)(r)]],
	["Grouped aggregate", "SELECT k, count(*), sum(v) FROM t GROUP BY k ORDER BY k", () => []],
	["ORDER BY v DESC LIMIT 20", "SELECT id, v FROM t ORDER BY v DESC LIMIT 20", () => []],
	["Whole-table count", "SELECT count(*) FROM t", () => []],
	["min/max whole table", "SELECT min(v), max(v) FROM t", () => []],
	["BETWEEN range", "SELECT count(*) FROM t WHERE v BETWEEN ? AND ?", (r) => { const a = rnd(900000)(r); return [a, a + 100000]; }],
	["IN list", "SELECT count(*) FROM t WHERE k IN (1,3,5,7)", () => []],
	["LIKE scan", "SELECT count(*) FROM t WHERE payload LIKE '%-7-payload'", () => []],
	["DISTINCT", "SELECT count(*) FROM (SELECT DISTINCT k FROM t)", () => []],
	["Rowid lookup", "SELECT v, payload FROM t WHERE id = ?", (r) => [1 + rnd(N)(r)]],
	["Secondary-index equality", "SELECT id, v FROM t WHERE sec = ?", (r) => [rnd(N)(r)]],
	["Indexed equi-join", "SELECT count(*) FROM t JOIN b ON t.bid = b.id WHERE t.sec = ?", (r) => [rnd(N)(r)]],
	["Join projecting rows", "SELECT t.id, b.name FROM t JOIN b ON t.bid = b.id WHERE t.sec = ?", (r) => [rnd(N)(r)]],
];

// mulberry32: a small seeded generator.
export function seeded(seed) {
	return () => {
		seed |= 0; seed = (seed + 0x6d2b79f5) | 0;
		let x = Math.imul(seed ^ (seed >>> 15), 1 | seed);
		x = (x + Math.imul(x ^ (x >>> 7), 61 | x)) ^ x;
		return ((x ^ (x >>> 14)) >>> 0) / 4294967296;
	};
}
