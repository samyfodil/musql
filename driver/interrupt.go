package driver

import (
	"context"
	"errors"
	"fmt"

	"github.com/samyfodil/musql/engine"
)

// watchContext interrupts the connection's statement when ctx ends before it
// does, as sqlite3_interrupt does in C: the statement stops at its next loop
// with "interrupted". The returned func ends the watch, given the statement's
// error, and returns that error with ctx's own beside it when ctx's end is
// what interrupted it, so errors.Is finds context.DeadlineExceeded or
// context.Canceled as database/sql callers test for. A statement that starts
// after the watch ends is not interrupted by this ctx.
func (c *Conn) watchContext(ctx context.Context) func(error) error {
	if ctx.Done() == nil {
		return func(err error) error { return err }
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		c.interrupt.Store(true)
		close(done)
	})
	return func(err error) error {
		if !stop() {
			<-done
		}
		c.interrupt.Store(false)
		if errors.Is(err, engine.ErrInterrupted) && ctx.Err() != nil {
			return fmt.Errorf("%w (%w)", err, ctx.Err())
		}
		return err
	}
}
