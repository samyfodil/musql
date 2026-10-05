package replication

import (
	"sync"
	"time"
)

// HLC represents a Hybrid Logical Clock: 48-bit physical milliseconds (from Unix epoch)
// in the high bits, 16-bit logical counter in the low bits.
type HLC uint64

// physical returns the physical component (milliseconds since Unix epoch).
func (h HLC) physical() uint64 {
	return uint64(h) >> 16
}

// logical returns the logical counter component.
func (h HLC) logical() uint16 {
	return uint16(h)
}

// Clock maintains a hybrid logical clock for a local site.
type Clock struct {
	mu   sync.Mutex
	last HLC
	site string
}

// NewClock creates a new Clock for the given site identifier.
func NewClock(site string) *Clock {
	return &Clock{
		site: site,
	}
}

// Now generates a new HLC that is strictly greater than the last generated HLC
// and greater than the current wall clock. It increments the logical counter
// when the physical time does not advance.
func (c *Clock) Now() HLC {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := uint64(time.Now().UnixMilli())
	lastPhys := c.last.physical()

	var physical uint64
	var logical uint16

	if now > lastPhys {
		physical = now
		logical = 0
	} else {
		physical = lastPhys
		logical = c.last.logical() + 1
	}

	c.last = HLC((physical << 16) | uint64(logical))
	return c.last
}

// Merge advances the clock to be greater than the provided remote HLC and
// the current wall clock, bumping the logical counter as needed. It returns
// the new HLC value. It is thread-safe.
func (c *Clock) Merge(remote HLC) HLC {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := uint64(time.Now().UnixMilli())
	remotePhys := remote.physical()
	remoteLog := remote.logical()
	lastPhys := c.last.physical()
	lastLog := c.last.logical()

	// Canonical HLC merge: physical = max of the three; the logical counter
	// carries forward from whichever inputs share that winning physical, +1,
	// so the result is strictly greater than both `remote` and the prior local.
	physical := max(now, remotePhys, lastPhys)

	var logical uint16
	switch {
	case physical == lastPhys && physical == remotePhys:
		logical = max(lastLog, remoteLog) + 1
	case physical == lastPhys:
		logical = lastLog + 1
	case physical == remotePhys:
		logical = remoteLog + 1
	default:
		// physical == now, strictly greater than both stored clocks.
		logical = 0
	}
	// ponytail: 16-bit logical wraps after 65535 events in one frozen ms; a
	// real wall clock advances long before that. Widen the split if ever hit.

	c.last = HLC((physical << 16) | uint64(logical))
	return c.last
}

// TagLess implements lexicographic total order: (hlcA, siteA) < (hlcB, siteB).
// It returns true if (hlcA, siteA) is less than (hlcB, siteB).
func TagLess(aHLC HLC, aSite string, bHLC HLC, bSite string) bool {
	if aHLC != bHLC {
		return aHLC < bHLC
	}
	return aSite < bSite
}
