package snout

import (
	"context"
	"errors"
	"strings"
	"sync"
)

// fakeDB records every statement and behaves like the salt table for the
// salt queries, so tests can assert on exactly what would reach Postgres.
type fakeDB struct {
	mu        sync.Mutex
	execs     []execCall
	salts     map[string][]byte
	failOn    string // Exec fails when its SQL contains this
	failQuery bool
	insertErr error
	inserted  chan int
}

type execCall struct {
	sql  string
	args []any
}

func newFakeDB() *fakeDB {
	return &fakeDB{salts: map[string][]byte{}, inserted: make(chan int, 64)}
}

func (f *fakeDB) Exec(_ context.Context, sql string, args ...any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failOn != "" && strings.Contains(sql, f.failOn) {
		return errors.New("exec failed")
	}
	f.execs = append(f.execs, execCall{sql: sql, args: args})
	switch {
	case strings.HasPrefix(sql, "INSERT INTO snout.salt"):
		day := args[0].(string)
		if _, ok := f.salts[day]; !ok {
			f.salts[day] = args[1].([]byte)
		}
	case strings.HasPrefix(sql, "DELETE FROM snout.salt"):
		for d := range f.salts {
			if d < args[0].(string) {
				delete(f.salts, d)
			}
		}
	case strings.HasPrefix(sql, "INSERT INTO snout.events"):
		if f.insertErr != nil {
			f.execs = f.execs[:len(f.execs)-1]
			return f.insertErr
		}
		f.inserted <- len(args) / len(eventColumns)
	}
	return nil
}

func (f *fakeDB) QueryRow(_ context.Context, _ string, args ...any) Row {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failQuery {
		return fakeRow{err: errors.New("query failed")}
	}
	return fakeRow{val: f.salts[args[0].(string)]}
}

// insertedArgs returns the args of every events INSERT so far.
func (f *fakeDB) insertedArgs() [][]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]any
	for _, c := range f.execs {
		if strings.HasPrefix(c.sql, "INSERT INTO snout.events") {
			out = append(out, c.args)
		}
	}
	return out
}

type fakeRow struct {
	val []byte
	err error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*(dest[0].(*[]byte)) = r.val
	return nil
}

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, errors.New("no entropy") }
