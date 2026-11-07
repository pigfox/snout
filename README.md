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

One dependency: [maxminddb-golang](https://github.com/oschwald/maxminddb-golang)
v1 (pure Go), for the optional location database. Everything else is the
standard library.

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
- No location without a source: trusted upstream headers or a local
  MaxMind-format database (see [Location](#location)). With neither, the
  location columns are NULL. Location is never finer than a city, and an IP
  database is wrong for some visitors (VPNs, mobile carriers, corporate
  proxies), most often at city level.
- Bots are filtered by User-Agent on the server. The User-Agent filter alone
  cannot catch automated browsers: headless Chrome, Lighthouse 13, Playwright
  and Puppeteer can send a plain Chrome User-Agent (Lighthouse 13.5.0 sends a
  moto g power Android UA on mobile and a macOS one on desktop). Two more
  checks keep them out:
  - the script sends nothing when `navigator.webdriver` is true, or when
    `navigator.userAgentData.brands` has no GREASE brand (`Not(A:Brand` and
    its variants);
  - the collector drops a beacon whose `Sec-CH-UA` header lists brands but
    no GREASE brand.

  Every Chromium browser adds a GREASE brand; Lighthouse's emulated brand
  list leaves it out. Firefox and Safari send no brand list and are judged
  by User-Agent alone. A robot that hides webdriver, copies a real brand list
  and lies about its User-Agent is counted. `SNOUT_BROWSER_TESTS=1 go test
  -run Browser .` checks this against real Lighthouse 13.5.0 and real Chrome.

## Wire it

1. **Schema.** Apply `snout.Schema` (the same text as `schema.sql`) with your
   migration tool. Every statement is idempotent. snout never runs DDL itself.

   **Upgrading from v0.1.x:** apply the v0.2.0 `snout.Schema` once more. It
   only adds: the `continent`, `region` and `city` columns (existing rows read
   NULL), the `snout.daily_geo` view, and the same three columns at the end of
   `snout.sessions`. Existing queries and views keep working.

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

## Location

Each row records where the visit came from: `continent` (two-letter code:
AF, AN, AS, EU, NA, OC, SA), `country` (ISO 3166-1 alpha-2), `region` (the
subdivision's name, such as a state) and `city` (name). Nothing finer:
latitude, longitude and postal code are never read, so they are never stored.

The raw IP is used for the lookup in memory, as it is for the visitor hash
and the rate limiter. It is never written to a row or a log, and lookup
errors, whose text can carry the IP, are dropped rather than passed to
`OnError`.

Two sources, in this order:

1. **Trusted upstream headers.** `CountryHeader`, `ContinentHeader`,
   `RegionHeader` and `CityHeader` name them. Behind Cloudflare:

   ```go
   snout.Config{
       CountryHeader:   snout.CloudflareCountryHeader,   // CF-IPCountry, every plan
       ContinentHeader: snout.CloudflareContinentHeader, // CF-IPContinent
       RegionHeader:    snout.CloudflareRegionHeader,    // CF-Region
       CityHeader:      snout.CloudflareCityHeader,      // CF-IPCity
   }
   ```

   Cloudflare sends the last three only with the managed transform **Add
   visitor location headers** on (Rules → Transform Rules → Managed
   Transforms). Configure a header only if the origin accepts traffic from the
   proxy alone; otherwise a visitor can write their own location. Invalid
   values are dropped: a country that is not two letters, Cloudflare's `XX`
   (unknown) and `T1` (Tor), a continent outside the seven codes, and names
   that are not valid UTF-8 or contain control characters.

2. **A local database file**, `Config.GeoDB`: the path of a MaxMind-format
   `.mmdb` City (or Country) database. The intended file is DB-IP's free
   [IP to City Lite](https://db-ip.com/db/download/ip-to-city-lite) mmdb,
   refreshed monthly. MaxMind's GeoLite2 City works the same way, under its
   own licence.

   - It answers when the headers carry no country. When they do, it fills
     only the fields the headers left empty, and only if it agrees on the
     country, so a row never mixes two countries' places.
   - The file is read into memory, not memory-mapped, so a file rewritten in
     place can never crash a reader. Memory use is about the size of the
     file, twice that briefly during a reload.
   - Every `GeoDBCheck` (default one minute) snout compares the file's size
     and modification time and reloads it when they change, without a
     restart. To update it, download the new month's file and move it over
     the old path (`mv` is atomic on one filesystem).
   - A missing, unreadable or corrupt file turns location off (NULLs) and is
     reported once to `OnError`; it never stops the collector. snout keeps
     checking, and picks the file up when a good one appears.

### Attribution (DB-IP Lite)

DB-IP's Lite databases are licensed under
[CC BY 4.0](https://creativecommons.org/licenses/by/4.0/). DB-IP asks that
wherever its data is used, the site link to it, for example:

```html
<a href="https://db-ip.com">IP Geolocation by DB-IP</a>
```

snout never injects anything into pages, so it does not add this link. A site
that uses the DB-IP file must add it itself (a footer or its privacy page are
the usual places). Whether and where is the site owner's decision. Headers
alone (Cloudflare) need no DB-IP attribution.

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
| `snout.daily_geo` | Demographic details: continent, country, region, city; NULL reads `(not set)` |
| `snout.sessions` | (one row per session, the base of the above) |

An engaged session follows GA4: at least ten seconds of engaged time or at
least two pageviews. Bounce rate is the share of sessions that were not
engaged.

## Grafana Cloud setup

The dashboard reads the views directly from Postgres, so Grafana needs a
read-only database role and a network path to the database. The names below
(`grafana_user`, `appdb`, `db.example.com`) are examples; substitute your own.

### 1. A read-only role

```sql
CREATE ROLE grafana_user WITH LOGIN PASSWORD '<choose a password>';
```

On a managed Postgres service you may have to create the role in the
provider's console instead; either way, give it no privileges beyond the
grants below.

### 2. Grants, in this order

```sql
-- Usually needs the database owner or an admin role, not the app user.
GRANT CONNECT ON DATABASE appdb TO grafana_user;

-- These can be run by the role that owns the snout schema (normally the role
-- your migrations run as).
GRANT USAGE ON SCHEMA snout TO grafana_user;
GRANT SELECT ON ALL TABLES IN SCHEMA snout TO grafana_user;
ALTER DEFAULT PRIVILEGES IN SCHEMA snout GRANT SELECT ON TABLES TO grafana_user;
```

`ALL TABLES` includes the views. The default-privileges line makes tables and
views added to `snout` by a later version readable without another grant, but
only for objects created by the role that runs it. Run it as the role that
applies your migrations.

### 3. Network allowlist

If the database only accepts connections from listed addresses, add Grafana
Cloud's outbound IPs:

1. Find your stack's region: Grafana Cloud portal → your stack → **Grafana
   Details**, the "cluster / region" value (for example `prod-us-west-0`).
2. Fetch that region's egress list:
   `https://allowlists.<region>.grafana.net/v1/grafana`, for example
   `https://allowlists.prod-us-west-0.grafana.net/v1/grafana`. The response is
   JSON; the addresses are in `service.ipv4`. Use the exact region from step 1:
   a short slug such as `us` can return a different region's list.
3. Add those addresses to the database's allowlist.

Don't use the legacy global list (`grafana.com/api/hosted-grafana/source-ips.txt`).
It covers every region and is deprecated after 2027-01-31.

These addresses can change. If a working connection starts timing out,
re-fetch the list and compare.

### 4. The data source

Connections → Data sources → Add → **PostgreSQL**:

| Field | Value |
| --- | --- |
| Host | `db.example.com:5432`, host and port only, with no scheme and no database name |
| Database | your app database, e.g. `appdb` |
| User / Password | `grafana_user` and its password |
| TLS/SSL Mode | `require` |
| TLS/SSL Method | File system path, with every certificate field left empty |
| Version | 15+ (or whatever your server runs) |
| TimescaleDB | off |
| Max open | `2`, or another low number |

Keep **Max open** low. A managed Postgres plan has a fixed connection limit,
and the app, its migrations and anything else on the cluster share it. A
dashboard that opens a connection per panel can use up the connections the
application needs. Two is plenty for one person refreshing a dashboard.

**Save & test** should report success before you import anything.

### 5. Troubleshooting

| Symptom | Cause | Fix |
| --- | --- | --- |
| Save & test spins, then times out | Network: Grafana's IPs are not allowed through | Step 3. Re-fetch the list for your exact region |
| `User does not have CONNECT privilege` | The CONNECT grant is missing | Run the first grant in step 2 as the database owner or an admin |
| `password authentication failed` | Wrong or stale password | Click **Reset** on the password field and enter it again. A saved password can't be edited in place |
| `permission denied for schema snout` | USAGE or SELECT is missing | Run the schema grants in step 2 as the schema's owner |

### 6. Import the dashboard

1. Dashboards → New → **Import dashboard**.
2. Upload `dashboards/overview.json`.
3. Pick the PostgreSQL data source from step 4.

The last row of panels shows location: users by continent, a geomap of
users by country, and the top countries and cities, all with the same time
range and hostname filter. The geomap draws OpenStreetMap tiles in the
viewer's browser and needs no plugin or API key.

The `site` and `hostname` variables fill from the data. `hostname` defaults to
the production host, so staging traffic stays out of the numbers unless you
select it.

**Data starts when snout is installed.** There is no backfill: traffic from
before the collector was mounted was never recorded, and nothing here can
reconstruct it. Compare against GA only over dates both were running.

## License

MIT
