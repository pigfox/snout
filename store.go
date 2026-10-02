package snout

import (
	"context"
	_ "embed"
	"strconv"
	"strings"
	"time"
)

// Schema is the DDL for the snout Postgres schema: the salt and events tables,
// their indexes and the reporting views. Every statement is idempotent. Apply
// it through your own migration tool; snout never runs DDL itself.
//
//go:embed schema.sql
var Schema string

// DB is the part of a Postgres pool snout uses. It is deliberately tiny so a
// caller adapts the pool it already has — a pgxpool.Pool needs a few lines,
// shown in the README — and snout never opens a second one.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) error
	QueryRow(ctx context.Context, sql string, args ...any) Row
}

// Row is a single-row query result.
type Row interface {
	Scan(dest ...any) error
}

// Event is one stored row of snout.events.
type Event struct {
	Site         string
	Hostname     string
	TS           time.Time
	VisitorHash  string
	SessionID    string
	Type         string
	Path         string
	Title        string
	ReferrerHost string
	UTMSource    string
	UTMMedium    string
	UTMCampaign  string
	UTMTerm      string
	UTMContent   string
	Device       string
	Browser      string
	OS           string
	ScreenBucket string
	Country      string
	EngagedMS    int64
	Name         string
	Props        string
}

// eventColumns is the insert column order; eventArgs must match it.
var eventColumns = []string{
	"site", "hostname", "ts", "visitor_hash", "session_id", "type", "path",
	"title", "referrer_host", "utm_source", "utm_medium", "utm_campaign",
	"utm_term", "utm_content", "device", "browser", "os", "screen_bucket",
	"country", "engaged_ms", "event_name", "props",
}

// nullable maps an empty string to SQL NULL, so a missing value is NULL in the
// views rather than an empty string that groups as its own bucket.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func eventArgs(e Event) []any {
	var engaged any
	if e.EngagedMS > 0 {
		engaged = e.EngagedMS
	}
	return []any{
		e.Site, e.Hostname, e.TS, e.VisitorHash, e.SessionID, e.Type, e.Path,
		nullable(e.Title), nullable(e.ReferrerHost), nullable(e.UTMSource),
		nullable(e.UTMMedium), nullable(e.UTMCampaign), nullable(e.UTMTerm),
		nullable(e.UTMContent), nullable(e.Device), nullable(e.Browser),
		nullable(e.OS), nullable(e.ScreenBucket), nullable(e.Country), engaged,
		nullable(e.Name), nullable(e.Props),
	}
}

// insertSQL builds one multi-row INSERT for n events.
func insertSQL(n int) string {
	var b strings.Builder
	b.WriteString("INSERT INTO snout.events (")
	b.WriteString(strings.Join(eventColumns, ", "))
	b.WriteString(") VALUES ")
	p := 1
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteByte('(')
		for c, col := range eventColumns {
			if c > 0 {
				b.WriteString(", ")
			}
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(p))
			if col == "props" {
				b.WriteString("::jsonb")
			}
			p++
		}
		b.WriteByte(')')
	}
	return b.String()
}

func insertEvents(ctx context.Context, db DB, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	args := make([]any, 0, len(events)*len(eventColumns))
	for _, e := range events {
		args = append(args, eventArgs(e)...)
	}
	return db.Exec(ctx, insertSQL(len(events)), args...)
}

// Prune deletes raw events older than olderThan. Schedule it daily; the views
// read the raw events, so pruning is also what bounds the reporting window.
func Prune(ctx context.Context, db DB, olderThan time.Duration) error {
	return db.Exec(ctx,
		`DELETE FROM snout.events WHERE ts < now() - make_interval(secs => $1)`,
		olderThan.Seconds())
}
