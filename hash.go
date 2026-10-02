package snout

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync"
	"time"
)

const saltBytes = 32

// salter holds the current UTC day's salt. The database row is the source of
// truth: a fresh random salt is offered with ON CONFLICT DO NOTHING and then
// read back, so two processes racing at midnight agree on one salt, and a
// restart mid-day keeps hashing with the salt it had.
type salter struct {
	db   DB
	rand io.Reader

	mu   sync.Mutex
	day  string
	salt []byte
}

func (s *salter) get(ctx context.Context, now time.Time) ([]byte, error) {
	day := now.UTC().Format(time.DateOnly)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.day == day {
		return s.salt, nil
	}

	fresh := make([]byte, saltBytes)
	if _, err := io.ReadFull(s.rand, fresh); err != nil {
		return nil, err
	}
	if err := s.db.Exec(ctx,
		`INSERT INTO snout.salt (day, salt) VALUES ($1::date, $2) ON CONFLICT (day) DO NOTHING`,
		day, fresh); err != nil {
		return nil, err
	}
	var salt []byte
	if err := s.db.QueryRow(ctx,
		`SELECT salt FROM snout.salt WHERE day = $1::date`, day).Scan(&salt); err != nil {
		return nil, err
	}
	// Rotation: once today's salt exists, no earlier one may survive, or a
	// hash from yesterday could be recomputed and joined to today's.
	if err := s.db.Exec(ctx,
		`DELETE FROM snout.salt WHERE day < $1::date`, day); err != nil {
		return nil, err
	}
	s.day, s.salt = day, salt
	return salt, nil
}

// visitorHash is SHA-256(salt + site + ip + userAgent). A zero byte separates
// the fields so no two different tuples can concatenate to the same input.
func visitorHash(salt []byte, site, ip, userAgent string) string {
	h := sha256.New()
	h.Write(salt)
	for _, f := range []string{site, ip, userAgent} {
		h.Write([]byte{0})
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}
