package engine

// The FROM clause as C's parser builds it. parse.y's seltablist rule turns a
// parenthesized list into one of three things:
//
//	seltablist ::= stl_prefix LP seltablist RP as on_using
//
//   - spliced flat, when it leads its FROM clause with no alias and no ON or
//     USING after it;
//   - with one item, that item again, carrying only the outer alias;
//   - otherwise an SF_NestedFrom subquery "SELECT * FROM <list>", whose
//     columns are rebuilt by selectExpander and sqlite3ColumnsFromExprList.
//
// The parser keeps flattening the items into SelectStmt.From as it always
// has; cSrcNode records the tree beside it, so name resolution can follow C's
// (nested_from_bind.go).

// cSrcNode is one SrcItem of the SrcList C builds for a FROM clause.
type cSrcNode struct {
	// leaf is the FromItem.cID of a table, subquery or table-function item;
	// 0 for a nested group.
	leaf int
	// sub is a nested group's own FROM list.
	sub []cSrcNode
	// alias / hasAlias are a nested group's alias; a leaf's is its FromItem's.
	alias    string
	hasAlias bool

	// How this node joins the nodes before it, as written; meaningful only
	// when joinSet (every node but the first of its list).
	joinSet bool
	join    JoinKind
	natural bool
	cross   bool
	using   []string
	hasOn   bool
}

// joined records the join operator and constraint parsed onto it, the item
// that starts n.
func (n cSrcNode) joined(it FromItem) cSrcNode {
	n.joinSet = true
	n.join = it.Join
	n.natural = it.Natural
	n.cross = it.CrossKeyword
	n.using = it.Using
	n.hasOn = it.On != nil
	return n
}

// cParenNodes is parse.y's seltablist rule for "( inner ) [AS alias]" at a
// position that is leading or not.
func cParenNodes(inner []cSrcNode, leading bool, alias string, hasAlias bool) []cSrcNode {
	if leading && !hasAlias {
		return inner
	}
	if len(inner) == 1 {
		n := inner[0]
		n.joinSet, n.join, n.natural, n.cross, n.using, n.hasOn = false, 0, false, false, nil, false
		n.alias, n.hasAlias = alias, hasAlias
		return []cSrcNode{n}
	}
	return []cSrcNode{{sub: inner, alias: alias, hasAlias: hasAlias}}
}

// cFromHasNested reports whether a FROM tree holds an SF_NestedFrom group.
func cFromHasNested(nodes []cSrcNode) bool {
	for i := range nodes {
		if nodes[i].leaf == 0 {
			return true
		}
	}
	return false
}
