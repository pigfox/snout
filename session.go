package snout

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"
	"time"
)

// SessionTimeout is the inactivity gap that ends a session, as in GA.
const SessionTimeout = 30 * time.Minute

// sessions assigns a session id from a visitor hash and the event time: an
// event within SessionTimeout of the visitor's previous one continues that
// session, anything later starts a new one whose id is derived from the hash
// and the start time. State is in memory and bounded by max; a restart, or a
// full table, starts new sessions rather than blocking or growing.
type sessions struct {
	mu   sync.Mutex
	max  int
	open map[string]session
}

type session struct {
	id   string
	seen time.Time
}

func newSessions(max int) *sessions {
	return &sessions{max: max, open: make(map[string]session)}
}

func (s *sessions) assign(visitor string, ts time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cur, ok := s.open[visitor]; ok && ts.Sub(cur.seen) < SessionTimeout {
		if ts.After(cur.seen) {
			cur.seen = ts
			s.open[visitor] = cur
		}
		return cur.id
	}

	sum := sha256.Sum256([]byte(visitor + "\x00" + strconv.FormatInt(ts.UnixNano(), 10)))
	id := hex.EncodeToString(sum[:16])

	if len(s.open) >= s.max {
		s.sweep(ts)
	}
	if len(s.open) < s.max {
		s.open[visitor] = session{id: id, seen: ts}
	}
	return id
}

// sweep drops sessions that have already timed out at now.
func (s *sessions) sweep(now time.Time) {
	for k, v := range s.open {
		if now.Sub(v.seen) >= SessionTimeout {
			delete(s.open, k)
		}
	}
}
