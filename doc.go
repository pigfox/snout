// Package snout is cookieless, first-party web analytics for Go servers that
// already hold a Postgres pool.
//
// A small script (Script) sends a pageview beacon, an engaged-time beacon when
// the page is hidden, and any custom events, to a collector (Tracker) mounted
// on the site's own origin. The collector hashes each visitor as
//
//	SHA-256(daily salt + site + client IP + User-Agent)
//
// where the salt is random, shared by every process through the snout.salt
// table, and replaced at 00:00 UTC with the previous day's row deleted. No
// cookie, no localStorage and no persistent identifier is ever set, and the raw
// IP is used only inside that hash and the in-memory rate limiter: it is never
// written to a row and never logged. The cost of that is stated rather than
// hidden — a visitor who returns tomorrow is a new visitor.
//
// Events are buffered in memory and written in batches through the caller's
// own pool (see DB), so snout never opens a connection of its own. A full
// buffer drops the event and counts it; a request is never blocked on the
// database.
//
// The schema ships as Schema (schema.sql) for the caller to apply through its
// own migration tool, with views that mirror GA4's default reports, and Prune
// deletes raw events past the caller's retention window.
package snout
