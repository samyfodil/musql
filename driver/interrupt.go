package driver

import "context"

// watchContext interrupts the connection's statement when ctx ends before it
// does, as sqlite3_interrupt does in C: the statement stops at its next loop
// with "interrupted". The returned func ends the watch; a statement that
// starts after it is not interrupted by this ctx.
func (c *Conn) watchContext(ctx context.Context) func() {
	if ctx.Done() == nil {
		return func() {}
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		c.interrupt.Store(true)
		close(done)
	})
	return func() {
		if !stop() {
			<-done
		}
		c.interrupt.Store(false)
	}
}
