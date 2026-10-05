// This file tests WHERE OR on unindexed tables with no ROWID/PRIMARY KEY references.
package compat

import "testing"

func TestR39NoRowidOrAnchor(t *testing.T) {
	differ(t, "r39_norowid_or_anchor", []string{
		`CREATE TABLE t1(a,b,c,d,e,f)`,
		`INSERT INTO t1 VALUES(100,200,300,400,500,600)`,
		`SELECT case d when -e+coalesce((select max(case (select +abs((~case case count(distinct b) when -count(*) then count(*) else max(t1.e) end when max(e) then min(t1.c) else count(*) end-min(c))) from t1) when t1.c then t1.a else -11 end) from t1 where case ~t1.b when e then d else 11 end in (b,t1.e,t1.b) or not t1.f>=b or (t1.f in (a,e,t1.e)) or f<b),t1.e) then t1.b else f end FROM t1 WHERE b<=(d) and (abs(coalesce((select max(coalesce((select case when (abs(~11)/abs(((abs(17+f+t1.f)/abs(t1.c))-11*c)-d*17)) not in (b,13,((e))) then 19 else 19 end from t1 where a in (select t1.b from t1 union select t1.b from t1)),13)) from t1 where t1.d in (select a from t1 union select 13 from t1)), -e)+17*c)/abs(t1.c))=13`,
	})
}

// TestR39RowidOrStaysDeclined is a sanity check that the new lever leaves a
// rowid-referencing OR's answer untouched: exprReferencesRowidColumn must
// still answer true for it, so anchorSingleTableOrsCannotYieldLTerm never
// even applies and whatever OTHER mechanism already served this shape
// (wherePlanIndexOrderDecided/where_plan_multior_r37a.go, which ports
// whereLoopAddOr's own concatenation order for cases the ported planner CAN
// decide) keeps deciding it exactly as before.
func TestR39RowidOrStaysDeclined(t *testing.T) {
	differ(t, "r39_rowid_or_anchor", []string{
		`CREATE TABLE t1(a,b,c,d,e,f)`,
		`INSERT INTO t1 VALUES(1,10,100,1,1,1)`,
		`INSERT INTO t1 VALUES(2,20,200,2,2,2)`,
		`INSERT INTO t1 VALUES(3,30,300,3,3,3)`,
		`SELECT group_concat(a), f FROM t1 WHERE rowid>1 OR rowid<1`,
	})
}

// TestR39SubqueryOperandOrAnchor gates exprReferencesRowidColumn's
// SubqueryExpr/ExistsExpr/InExpr.Sub handling: neither is itself a TK_COLUMN
// node, so neither can ever be the operand exprMightBeIndexed
// (whereexpr.c:1198-1220) matches to sPk's rowid column -- an IN's own
// tested operand (x.X) is all that matters, never what its subquery
// produces. Mined from randexpr1.test#0 statement #479 (real corpus shape,
// "a*b*11 in (select ... union select e from t1)"); the minimal repro below
// isolates the InExpr.Sub relaxation specifically, since #479 alone does not
// discriminate it (verified: reverting only InExpr.Sub left #479 passing,
// because #479's OUTER decline actually turns on a SEPARATE nested
// SubqueryExpr operand elsewhere in its WHERE).
func TestR39SubqueryOperandOrAnchor(t *testing.T) {
	differ(t, "r39_subquery_operand_or_anchor", []string{
		`CREATE TABLE t1(a,b,c,d,e,f)`,
		`INSERT INTO t1 VALUES(1,10,100,1,1,1)`,
		`INSERT INTO t1 VALUES(2,20,200,2,2,2)`,
		`INSERT INTO t1 VALUES(3,30,300,3,3,3)`,
		`SELECT group_concat(a), f FROM t1 WHERE c IN (SELECT c FROM t1 WHERE c=300) OR d=1`,
	})
	differ(t, "r39_subquery_operand_or_anchor_479", []string{
		`CREATE TABLE t1(a,b,c,d,e,f)`,
		`INSERT INTO t1 VALUES(100,200,300,400,500,600)`,
		`SELECT case when ((select  -min(e)+cast(avg(t1.a) AS integer) from t1)>t1.d*a) then (abs(~a)/abs(+11))*f when (c-(coalesce((select 11 from t1 where t1.d in (case when (19)+11>=c then 19 when e<>b then 13 else c end,a,13)),17))+13) not between 11 and t1.c then (t1.b) else 17 end+b FROM t1 WHERE a*b*11 in (select case when not coalesce((select max(~13 | (select abs((count(distinct t1.f))*cast(avg(t1.f) AS integer)+(max(t1.e))) from t1)) from t1 where coalesce((select max(11) from t1 where t1.b=19),b) in (f,11,19) and t1.f between a and t1.c or 19=t1.a and d=17 or c>=t1.f or t1.d<>t1.c),11)*c in (select f from t1 union select  -t1.b from t1) then t1.c when c not between f and 13 then e else  -e end from t1 union select e from t1)`,
	})
}
