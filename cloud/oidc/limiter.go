package oidc

import (
	"sync"
	"time"
)

// Defaults for the failed-login limits.
const (
	DefaultLoginPerIP    = 20
	DefaultLoginPerEmail = 5
	DefaultLoginWindow   = 15 * time.Minute
)

// limiter counts failures per key within a sliding window, in memory: a
// restart forgets them, and a second node counts on its own.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	fails  map[string][]time.Time
}

func newLimiter(max int, window time.Duration) *limiter {
	return &limiter{max: max, window: window, now: time.Now, fails: map[string][]time.Time{}}
}

// recent drops the failures older than the window and returns the rest.
func (l *limiter) recent(key string) []time.Time {
	cut := l.now().Add(-l.window)
	ts := l.fails[key]
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = ts
	return ts
}

// blocked reports whether key is at its limit, and how long until the oldest
// failure leaves the window.
func (l *limiter) blocked(key string) (time.Duration, bool) {
	if l.max <= 0 {
		return 0, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	ts := l.recent(key)
	if len(ts) < l.max {
		return 0, false
	}
	return ts[0].Add(l.window).Sub(l.now()), true
}

func (l *limiter) fail(key string) {
	if l.max <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) > 10000 {
		for k := range l.fails {
			l.recent(k)
		}
	}
	l.fails[key] = append(l.recent(key), l.now())
}

func (l *limiter) reset(key string) {
	l.mu.Lock()
	delete(l.fails, key)
	l.mu.Unlock()
}
