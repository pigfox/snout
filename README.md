# snout

Cookieless, first-party web analytics for a Go server that already has a
Postgres pool. A small script beacons pageviews, engaged time and custom
events to a collector on your own origin; the collector writes them in batches
through your pool; views in Postgres turn them into GA4-shaped daily reports;
a Grafana dashboard reads the views. Extracted from
[pigfox.com](https://pigfox.com), where it runs beside Google Analytics so the
two can be compared like for like.

```
go get github.com/pigfox/snout
```

Standard library only.

## The cookieless method

Nothing is stored in the browser — no cookie, no localStorage, no
sessionStorage, no fingerprint, no identifier of any kind. The collector names
a visitor with

```
SHA-256(daily salt + site + client IP + User-Agent)
```

- The salt is 32 random bytes per UTC day, kept in `snout.salt` so every
  process and every restart hashes a day with the same salt.
- At 00:00 UTC a new salt is created and the previous day's row is deleted, so
  yesterday's hashes can never be recomputed or joined to today's.
- The raw IP is used only inside that hash and an in-memory rate limiter (keyed
  by a hash of the IP). It is never written to a row and never logged.
- A session ends after 30 minutes of inactivity; its id is derived from the
  visitor hash and the session's start time.

Only the five `utm_` query keys ever leave the page. The referrer is reduced to
its host, and screen width to a coarse bucket.

## What it cannot do

- **No returning visitors across days.** The salt rotates daily by design, so a
  person who comes back tomorrow is a new user. Daily users are accurate;
  weekly or monthly "unique users" are a sum of daily ones and over-count.
- Two people behind one IP with the same browser build are one visitor.
- Sessions are tracked in memory, so a restart splits sessions that were open.
- No country without a trusted upstream header (`Config.CountryHeader`); there
  is no GeoIP database.
- Bots are filtered by User-Agent. A robot that lies about its User-Agent and
  runs JavaScript is counted.

## Wire it

1. **Schema.** Apply `snout.Schema` (the same text as `schema.sql`) with your
   migration tool. Every statement is idempotent. snout never runs DDL itself.

2. **Adapt your pool.** snout asks for two methods, so it uses the pool you
   already have and never opens connections of its own. For pgx:

   ```go
   type snoutDB struct{ pool *pgxpool.Pool }

   func (d snoutDB) Exec(ctx context.Context, sql string, args ...any) error {
       _, err := d.pool.Exec(ctx, sql, args...)
       return err
   }

   func (d snoutDB) QueryRow(ctx context.Context, sql string, args ...any) snout.Row {
       return d.pool.QueryRow(ctx, sql, args...)
   }
   ```

3. **Mount the collector.**

   ```go
   tracker, err := snout.New(snoutDB{pool}, snout.Config{
       Site:     "example",
       Hosts:    []string{"example.com", "staging.example.com"},
       ClientIP: clientIP, // the function your app already trusts behind its proxy
   })
   mux.Handle(snout.DefaultPath, tracker)
   ```

   Pick a neutral path: ad-blocker lists match words like `analytics`,
   `collect` and `track`. Call `tracker.Close(ctx)` on shutdown to write what is
   still buffered.

4. **Serve the script** (`snout.Script`) from your own origin and load it on
   every page:

   ```html
   <script defer src="/js/snout.js" data-endpoint="/_s" nonce="..."></script>
   ```

   Custom events: `snout.event("signup", {plan: "pro"})`.

5. **Prune** raw events daily: `snout.Prune(ctx, db, 395*24*time.Hour)`. The
   views read raw events, so this also bounds the reporting window.

The writer buffers in memory (`BufferSize`), writes a batch every `BatchSize`
events or `FlushInterval`, and never blocks a request: a full buffer drops the
event and counts it in `Stats()`.

Every row records the request's hostname, so a staging host that shares the
database is separated by filtering on it — which the dashboard does.

## Reports

| View | GA4 equivalent |
| --- | --- |
| `snout.daily` | Reports snapshot: users, sessions, views, engaged sessions, bounce rate, average engagement time per user |
| `snout.daily_pages` | Pages and screens |
| `snout.daily_entry_pages`, `snout.daily_exit_pages` | Landing / exit pages |
| `snout.daily_sources` | Traffic acquisition, session source / medium |
| `snout.daily_campaigns` | Session campaign |
| `snout.daily_tech` | Tech details: device category, browser, OS |
| `snout.daily_events` | Events |
| `snout.sessions` | (one row per session, the base of the above) |

An engaged session follows GA4: at least ten seconds of engaged time or at
least two pageviews. Bounce rate is the share of sessions that were not
engaged.

## Grafana

1. Add a PostgreSQL data source with a role that can read the schema:
   `GRANT USAGE ON SCHEMA snout`, `SELECT` on its tables and views.
2. Dashboards → New → Import, upload `dashboards/overview.json`.
3. Choose the data source. The `site` and `hostname` variables fill from the
   data; `hostname` defaults to the production host, so staging traffic stays
   out of the numbers unless you select it.

## License

MIT
