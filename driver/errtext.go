package driver

import (
	"strings"
)

// parityErrText strips layer prefixes ("engine: ", "vdbe: ", "driver: ") from
// error messages. Prefixes can nest, so this loops until none remain from the
// front. A prefix appearing mid-message is left alone (see parityErr's comment).
func parityErrText(s string) string {
	for {
		before := s
		for _, p := range []string{"engine: ", "vdbe: ", "driver: "} {
			s = strings.TrimPrefix(s, p)
		}
		s = stripStatementContext(s)
		if s == before {
			return s
		}
	}
}

// stripStatementContext removes statement and table name prefixes that the
// engine prepends to write errors ("INSERT into t: ", "UPDATE t: ", etc).
// C SQLite does not include these. Only a bare token name is removed, and only
// from the front, so this cannot damage a message containing these words elsewhere.
func stripStatementContext(s string) string {
	// DDL verbs that name the object; " AS SELECT" is consumed as part of the verb.
	for _, verb := range []string{"INSERT into ", "UPDATE ", "DELETE from ",
		"CREATE INDEX ", "CREATE TABLE ", "CREATE VIEW ", "CREATE TRIGGER ",
		"CREATE VIRTUAL TABLE "} {
		// Bare verb with colon (no object name) is also removed.
		if bare := strings.TrimSuffix(verb, " ") + ": "; strings.HasPrefix(s, bare) {
			return s[len(bare):]
		}
		if !strings.HasPrefix(s, verb) {
			continue
		}
		rest := s[len(verb):]
		i := strings.Index(rest, ": ")
		if i <= 0 {
			continue
		}
		name := strings.TrimSuffix(rest[:i], " AS SELECT")
		if name == "" || strings.ContainsAny(name, " \t") {
			continue // not a bare object name -- leave the message alone
		}
		return rest[i+2:]
	}
	return s
}

// parityError is an error with a normalized message but the original cause,
// so Unwrap keeps errors.Is/As working.
type parityError struct {
	msg   string
	cause error
}

func (e *parityError) Error() string { return e.msg }
func (e *parityError) Unwrap() error { return e.cause }

// parityErr normalizes error messages at the database/sql boundary. It must
// not touch nil or sentinel errors (driver.ErrSkip, driver.ErrBadConn, io.EOF),
// which are returned unchanged if their message is unchanged. Errors with
// prefixes mid-message (nested wrappers) are left alone.
func parityErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	clean := parityErrText(msg)
	if clean == msg {
		return err
	}
	return &parityError{msg: clean, cause: err}
}
