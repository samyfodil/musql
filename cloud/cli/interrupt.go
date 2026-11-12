package cli

import (
	"bytes"
	"context"
	"io"
	"sync"
)

// keyPump is the one reader of an interactive terminal. The line editor reads
// the keys through it; while a statement runs, a Ctrl+C among them interrupts
// the statement instead, and any other key waits for the editor. In raw mode
// the terminal sends Ctrl+C as a byte, not a signal.
type keyPump struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	err    error
	cancel context.CancelFunc
}

func newKeyPump(r io.Reader) *keyPump {
	p := &keyPump{}
	p.cond = sync.NewCond(&p.mu)
	go p.pump(r)
	return p
}

func (p *keyPump) pump(r io.Reader) {
	b := make([]byte, 256)
	for {
		n, err := r.Read(b)
		data := b[:n]
		p.mu.Lock()
		if p.cancel != nil && bytes.IndexByte(data, 0x03) >= 0 {
			p.cancel()
			data = bytes.ReplaceAll(data, []byte{0x03}, nil)
		}
		p.buf = append(p.buf, data...)
		p.err = err
		p.cond.Broadcast()
		p.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (p *keyPump) Read(dst []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buf) == 0 && p.err == nil {
		p.cond.Wait()
	}
	if len(p.buf) == 0 {
		return 0, p.err
	}
	n := copy(dst, p.buf)
	p.buf = p.buf[n:]
	return n, nil
}

// watch makes a Ctrl+C call cancel until the returned func is called.
func (p *keyPump) watch(cancel context.CancelFunc) (stop func()) {
	p.mu.Lock()
	p.cancel = cancel
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		p.cancel = nil
		p.mu.Unlock()
	}
}
