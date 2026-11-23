package server

import (
	"context"
	"errors"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/samyfodil/musql/hrana"

	"github.com/samyfodil/musql/cloud/state"
)

// Access is what an access policy lets clients do to a database, on top of
// what their tokens grant.
type Access int

const (
	// Allow leaves clients what their tokens grant.
	Allow Access = iota
	// ReadOnly refuses every statement that writes, DDL included.
	ReadOnly
	// Deny refuses every statement. Any other value is taken as Deny.
	Deny
)

// Policy is an access policy's answer for a database. Reason is what a
// refused client is told.
type Policy struct {
	Access Access
	Reason string
}

// WithAccessPolicy limits what clients may do to an org's databases beyond
// their tokens, as a lapsed subscription or a used-up quota calls for. Under
// ReadOnly reads work and writes fail; under Deny every statement fails; both
// over Hrana with the error code BLOCKED and "Operation was blocked:
// <Reason>", as a database's own block does, the console's included.
//
// fn is asked for a database when a Hrana client first reaches it and again
// once its answer is a second old, at the next statement, so a stream already
// open meets a change within a second. It is also asked by the calls that
// read or write a database's data: Export and AI SQL need the database not
// denied, Restore in place needs it allowed, and Create and Restore into a new
// database need the org allowed, asked with db nil, and the database they copy
// not denied. A refused call fails with FAILED_PRECONDITION and Reason. Every
// other management call (databases, groups, tokens, members, uploads) stays
// open, so an org can still fix what got it refused.
//
// fn runs on the statement path: it should answer from the embedder's cache.
func WithAccessPolicy(fn func(ctx context.Context, org *state.Org, db *state.Database) Policy) Option {
	return func(s *Server) { s.accessPolicy = fn }
}

// level is p.Access with every unknown value taken as Deny.
func (p Policy) level() Access {
	if p.Access == Allow || p.Access == ReadOnly {
		return p.Access
	}
	return Deny
}

func (p Policy) reason() string {
	switch {
	case p.Reason != "":
		return p.Reason
	case p.level() == ReadOnly:
		return "writes to this database are not allowed"
	}
	return "access to this database is denied"
}

// over is the block clients run under with p on top of the database's own b:
// the stronger refusal wins, and p's reason on a tie.
func (p Policy) over(b hrana.Block) hrana.Block {
	switch {
	case p.level() == Deny:
		return hrana.Block{Reads: true, Writes: true, Reason: p.reason()}
	case p.level() == ReadOnly && !b.Reads:
		return hrana.Block{Writes: true, Reason: p.reason()}
	}
	return b
}

// admit refuses a management call on o's database d (nil: a new one) when
// the access policy's answer is stricter than upTo.
func (s *Server) admit(ctx context.Context, o *state.Org, d *state.Database, upTo Access) error {
	if s.accessPolicy == nil {
		return nil
	}
	p := s.accessPolicy(ctx, o, d)
	if p.level() > upTo {
		return connect.NewError(connect.CodeFailedPrecondition, errors.New(p.reason()))
	}
	return nil
}

// policyTTL is how long an open database keeps the access policy's answer.
const policyTTL = time.Second

// policyCache is the access policy's last answer for one open database.
type policyCache struct {
	ask func() (Policy, error)
	mu  sync.Mutex
	p   Policy
	at  time.Time
}

// get answers from the cache, asking again once the answer is policyTTL old.
// An ask that fails keeps the last answer until the next one is due.
func (c *policyCache) get() Policy {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at.IsZero() || time.Since(c.at) >= policyTTL {
		if p, err := c.ask(); err == nil {
			c.p = p
		}
		c.at = time.Now()
	}
	return c.p
}

// entryPolicy asks the access policy about e's database, as last recorded.
func (s *Server) entryPolicy(e *entry) (Policy, error) {
	ctx := context.Background()
	d := e.rec.Load()
	o, err := s.store.OrgByID(ctx, d.OrgID)
	if err != nil {
		return Policy{}, err
	}
	return s.accessPolicy(ctx, o, d), nil
}
