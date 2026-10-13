package engine

import "fmt"

// txnWholeStmtSyntaxErr is txnStmtSyntaxErr for BEGIN / COMMIT / END / a plain
// ROLLBACK: `near "X": syntax error` at the first token the grammar
// "verb [mode] [TRANSACTION [nm]] [;]" cannot take ("BEGIN CONCURRENT"). nil
// for anything else, so the caller keeps its own error.
func txnWholeStmtSyntaxErr(sqlText string, toks []token) error {
	p := newParser(sqlText, toks)
	switch {
	case p.consumeKeyword("BEGIN"):
		if !p.consumeKeyword("DEFERRED") && !p.consumeKeyword("IMMEDIATE") {
			p.consumeKeyword("EXCLUSIVE")
		}
	case p.consumeKeyword("COMMIT"), p.consumeKeyword("END"), p.consumeKeyword("ROLLBACK"):
	default:
		return nil
	}
	if p.consumeKeyword("TRANSACTION") {
		consumeTxnName(p)
	}
	if p.peekIsPunct(";") {
		p.next()
	}
	if t := p.peek(); t.kind != tkEOF && !(t.kind == tkIdent && !t.quoted && t.upper() == "TO") {
		return fmt.Errorf("engine: near %q: syntax error", t.text)
	}
	return nil
}
