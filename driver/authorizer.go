package driver

import (
	"database/sql/driver"

	"github.com/samyfodil/musql/engine"
)

// An Authorizer is engine.Authorizer: called with C SQLite's action codes
// (engine.AuthRead and the rest) and answering engine.AuthOK, AuthDeny or
// AuthIgnore.
type Authorizer = engine.Authorizer

// SetAuthorizer sets the callback this connection asks about every action of
// every statement it prepares from now on, as sqlite3_set_authorizer does; nil
// removes it. Statements prepared before are authorized again on their next
// run. Reach the connection through sql.Conn.Raw.
func (c *Conn) SetAuthorizer(fn Authorizer) {
	c.authorizer = fn
	c.authGen++
}

// authorize asks the connection's authorizer about s, once per authorizer it
// runs under, and reports the text to run: s's own or the one the authorizer
// rewrote. skip is a statement the authorizer made do nothing.
func (s *Stmt) authorize() (sqlText string, skip bool, err error) {
	c := s.conn
	if s.authGen == c.authGen {
		return s.runText(), s.authSkip, nil
	}
	s.authSQL, s.authSkip = "", false
	if c.authorizer != nil {
		c.stmtText = s.sqlText
		if err := c.ensureSession(); err != nil {
			return "", false, err
		}
		var attached []engine.AuthDB
		for _, at := range c.attached {
			nw, err := c.sessionFor(at.path)
			if err != nil {
				return "", false, err
			}
			attached = append(attached, engine.AuthDB{Name: at.name, DB: nw.DB})
		}
		v, err := c.ndb.Authorize(s.sqlText, c.authorizer, attached...)
		if err != nil {
			return "", false, err
		}
		s.authSQL, s.authSkip = v.SQL, v.Skip
	}
	s.authGen = c.authGen
	return s.runText(), s.authSkip, nil
}

func (s *Stmt) runText() string {
	if s.authSQL != "" {
		return s.authSQL
	}
	return s.sqlText
}

// skippedResult is what a statement the authorizer made do nothing returns.
func (s *Stmt) skippedResult() driver.Result {
	return &execResult{lastInsertID: s.conn.lastInsertRowid}
}
