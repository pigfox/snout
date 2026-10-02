package snout

import (
	"crypto/sha256"
	"sync"
	"time"
)

// limiter is a fixed-window counter per client. It is keyed by a hash of the
// IP rather than the IP itself, so not even the process memory holds a table
// of raw addresses; the whole table is discarded at every window boundary.
type limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	start  time.Time
	counts map[[sha256.Size]byte]int
}

func newLimiter(limit int, window time.Duration) *limiter {
	return &limiter{limit: limit, window: window, counts: make(map[[sha256.Size]byte]int)}
}

func (l *limiter) allow(ip string, now time.Time) bool {
	key := sha256.Sum256([]byte(ip))

	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.start) >= l.window || now.Before(l.start) {
		l.start = now
		l.counts = make(map[[sha256.Size]byte]int)
	}
	if l.counts[key] >= l.limit {
		return false
	}
	l.counts[key]++
	return true
}
