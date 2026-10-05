// The probe chain whereLoopAddBtree walks for a WITHOUT ROWID table.
//
// A WITHOUT ROWID table has no rowid for the fake sPk Index to stand for, so
// where.c does not build one at all: "else if( !HasRowid(pTab) ){ pProbe =
// pTab->pIndex; }" (where.c:4034-4035) starts the chain directly at the table's
// own real indexes, the last of which is the PRIMARY KEY index -- which for
// such a table IS the table ("The root page of the PRIMARY KEY is the table
// root page", build.c:2447-2448).
//
// convertToWithoutRowidTable (build.c:2354-2507) is what makes that PRIMARY KEY
// index reproducible without a sqlite_schema row of its own to read it from:
//
//   - every PRIMARY KEY column becomes NOT NULL (build.c:2362-2374) -- applied
//     here at the parse, see parseCreateTableColumnsAndAutoIndexes;
//   - isCovering = 1 and uniqNotNull = 1 unconditionally (build.c:2435-2436);
//   - nColumn is reset to nKeyCol and then EVERY remaining non-VIRTUAL table
//     column is appended, in table-declaration order, each under its own
//     declared collation (build.c:2437, 2485-2506) -- the same layout this
//     engine stores such a table's rows in;
//   - sqlite3EndTable then prices it (estimateIndexWidth, build.c:2784-2787).
package engine

import "strings"

// wherePlanWithoutRowidIndexList assembles whereLoopAddBtree's probe chain for a
// WITHOUT ROWID table. It answers ONLY for the chain that is exactly one index
// long -- the PRIMARY KEY index alone -- and declines every other WITHOUT ROWID
// table outright.
//
// That restriction is not a shortcut around the cost model: the cost model runs
// on this index exactly as it does on any other, through the same addBtree /
// addBtreeIndex this file does not duplicate. What it avoids is the part of
// convertToWithoutRowidTable/sqlite3CreateIndex that only a SECOND index can
// reach, and that this port does not reproduce:
//
//   - a secondary index over a WITHOUT ROWID table carries the PRIMARY KEY
//     columns appended behind its own key instead of a rowid, and only the ones
//     that are not already in that key under the same collation
//     (isDupColumn/build.c:2274-2293, applied at build.c:4274-4292) -- so its
//     b-tree ORDER, its colNotIdxed and its covering test all differ from the
//     rowid-table shape the loop in wherePlanIndexList builds;
//   - the automatic index a UNIQUE constraint on such a table creates is named
//     from a counter that the PRIMARY KEY index ITSELF advances while writing no
//     sqlite_schema row (build.c:4097-4101's "for(pLoop=pTab->pIndex, n=1; ...)"
//     against convertToWithoutRowidTable's schema-entry bypass, build.c:2439-2446),
//     so the ordinal wherePlanIndexList verifies is off by one in a way that
//     depends on WHERE the PRIMARY KEY sits among the constraints. Measured
//     against the 3.53.3 oracle: "CREATE TABLE w1(a,b,c, UNIQUE(b), PRIMARY
//     KEY(a)) WITHOUT ROWID" names it sqlite_autoindex_w1_1, while the same two
//     constraints in the other order name it sqlite_autoindex_w2_2.
//
// Both are index-chain SHAPE questions, and getting either wrong picks a
// different loop -- a different row ORDER -- than the oracle walks, which for
// this port's callers is a wrong answer rather than a slow one. A table with a
// second index therefore keeps the decline it has always had.
//
// MEASURED, 1,600 randomized two-table shapes (a WITHOUT ROWID table of 1-3
// PRIMARY KEY columns with varied declared types, per-column and
// constraint-level collations and DESC flags, joined to an indexed rowid table,
// read by a statement whose answer reports arrival order -- an aggregate
// anchor, a group_concat or a per-group anchor): ZERO cell divergences from the
// 3.53.3 oracle. Seven of the 1,600 differed in the ORDER the GROUP BY groups
// were EMITTED in, never in a cell. That was not this file's: it was
// groupsArriveOutOfKeyOrder (vdbe_agg_codegen.go), which reproduced select.c's
// groupBySort==0 arm only for a SINGLE-table FROM clause and so re-sorted a
// multi-table plan's groups ascending -- equally reachable on ORDINARY rowid
// tables, where over v(a,b) with "CREATE INDEX iv ON v(a DESC)", "SELECT
// count(*), v.a FROM t, v GROUP BY v.a" emits 3,2,1 on the oracle. That arm now
// covers a join too (wherePlanOrder stamps groupsSorted on every level).
func wherePlanWithoutRowidIndexList(p *ReadOnlyPager, tbl *resolvedTable, tableName, indexedBy string, notIndexed bool, szTabRow logEst) ([]whereIndexInfo, logEst, bool) {
	if indexedBy != "" || notIndexed {
		// INDEXED BY names an index by name, and the PRIMARY KEY index of a
		// WITHOUT ROWID table has one (sqlite3FindIndex reaches it through the
		// schema's idxHash) without having a sqlite_schema row to look it up
		// from here. NOT INDEXED is the stranger of the two: whereLoopAddBtree
		// consults pSrc->fg.notIndexed only inside the branch that builds sPk,
		// so for a WITHOUT ROWID table it truncates nothing at all. Neither is
		// reproduced; both decline.
		return nil, 0, false
	}
	if len(tbl.pkColIdx) == 0 {
		return nil, 0, false
	}
	rows, err := p.Schema()
	if err != nil {
		return nil, 0, false
	}
	tblSQL := ""
	for i := range rows {
		if rows[i].Type == "index" && equalFoldName(rows[i].TblName, tableName) {
			return nil, 0, false // a second index: see this function's doc comment
		}
		if rows[i].Type == "table" && equalFoldName(rows[i].Name, tableName) {
			tblSQL = rows[i].SQL
		}
	}
	if tblSQL == "" {
		return nil, 0, false
	}
	_, _, specs, perr := parseCreateTableColumnsAndAutoIndexes(tblSQL, true)
	if perr != nil || len(specs) != 1 || specs[0].kind != "pk" {
		// One constraint, and it is the PRIMARY KEY: anything else either
		// creates a second index (a UNIQUE constraint does, with a
		// sqlite_schema row the loop above would already have caught) or means
		// this parse and resolveTableIn's disagree about the table's shape.
		return nil, 0, false
	}
	spec := specs[0]
	if len(spec.cols) != len(tbl.pkColIdx) {
		return nil, 0, false
	}
	idx := whereIndexInfo{
		// "The root page of the PRIMARY KEY is the table root page"
		// (build.c:2447-2448).
		name: tableName, root: tbl.root,
		nKeyCol:     len(tbl.pkColIdx),
		onError:     true, // IsUniqueIndex: a PRIMARY KEY constraint
		uniqNotNull: true, // build.c:2436
		isCovering:  true, // build.c:2435
		primaryKey:  true,
	}
	inPK := make([]bool, len(tbl.cols))
	for k, ci := range tbl.pkColIdx {
		if ci < 0 || ci >= len(tbl.cols) || !equalFoldName(tbl.cols[ci].Name, spec.cols[k]) {
			// resolveTableIn maps the PRIMARY KEY spec's column NAMES to
			// indices and leaves 0 for a name it cannot find (query.go). A
			// disagreement here means the key this would build is not the one
			// the table stores, so decline rather than guess.
			return nil, 0, false
		}
		if inPK[ci] {
			// "Remove all redundant columns from the PRIMARY KEY"
			// (build.c:2417-2432) collapses a repeated column, which changes
			// nKeyCol. Not reproduced.
			return nil, 0, false
		}
		inPK[ci] = true
		coll := effectiveCollation(tbl.cols[ci].Collation)
		if k < len(spec.colls) && spec.colls[k] != "" {
			coll = strings.ToUpper(spec.colls[k])
		}
		idx.aiColumn = append(idx.aiColumn, ci)
		idx.azColl = append(idx.azColl, coll)
		idx.aSortOrder = append(idx.aSortOrder, k < len(spec.desc) && spec.desc[k])
		idx.exprs = append(idx.exprs, nil)
	}
	// "Add all table columns to the PRIMARY KEY index" (build.c:2485-2506):
	// every non-PRIMARY-KEY, non-VIRTUAL column, in declaration order, under
	// its own declared collation and ASCENDING (resizeIndexObject zeroes the
	// aSortOrder it grows, build.c:2190-2216).
	for ci := range tbl.cols {
		if inPK[ci] {
			continue
		}
		if tbl.cols[ci].IsGenerated() && !tbl.cols[ci].GeneratedStored {
			continue // COLFLAG_VIRTUAL
		}
		idx.aiColumn = append(idx.aiColumn, ci)
		idx.azColl = append(idx.azColl, effectiveCollation(tbl.cols[ci].Collation))
		idx.aSortOrder = append(idx.aSortOrder, false)
		idx.exprs = append(idx.exprs, nil)
	}
	whereDefaultRowEst(&idx)
	idx.szIdxRow = whereIndexSzRow(&idx, tbl.cols)
	whereRecomputeColumnsNotIndexed(&idx)
	return []whereIndexInfo{idx}, szTabRow, true
}
