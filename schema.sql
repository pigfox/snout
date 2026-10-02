-- snout schema v0.1.0. Apply through your own migration tool; every statement
-- is idempotent so re-applying it is harmless.

CREATE SCHEMA IF NOT EXISTS snout;

-- One random salt per UTC day. The row for a day is created by whichever
-- process sees that day's first event, so every instance and every restart
-- hashes with the same salt; the previous day's row is deleted on rotation.
CREATE TABLE IF NOT EXISTS snout.salt (
    day  date  PRIMARY KEY,
    salt bytea NOT NULL
);

CREATE TABLE IF NOT EXISTS snout.events (
    id            bigserial   PRIMARY KEY,
    site          text        NOT NULL,
    hostname      text        NOT NULL,
    ts            timestamptz NOT NULL,
    visitor_hash  text        NOT NULL,
    session_id    text        NOT NULL,
    type          text        NOT NULL CHECK (type IN ('pageview', 'engagement', 'event')),
    path          text        NOT NULL,
    title         text,
    referrer_host text,
    utm_source    text,
    utm_medium    text,
    utm_campaign  text,
    utm_term      text,
    utm_content   text,
    device        text,
    browser       text,
    os            text,
    screen_bucket text,
    country       text,
    engaged_ms    bigint,
    event_name    text,
    props         jsonb
);

CREATE INDEX IF NOT EXISTS events_site_host_ts_idx ON snout.events (site, hostname, ts);
CREATE INDEX IF NOT EXISTS events_session_idx ON snout.events (session_id);
CREATE INDEX IF NOT EXISTS events_ts_idx ON snout.events (ts);

-- One row per session. Engaged follows GA4: at least ten seconds of engaged
-- time, or at least two pageviews. Bounce rate is the share NOT engaged.
CREATE OR REPLACE VIEW snout.sessions AS
SELECT
    site,
    hostname,
    session_id,
    min(visitor_hash)                                                         AS visitor_hash,
    min(ts)                                                                   AS started,
    max(ts)                                                                   AS ended,
    count(*) FILTER (WHERE type = 'pageview')                                 AS pageviews,
    coalesce(sum(engaged_ms), 0)                                              AS engaged_ms,
    count(*) FILTER (WHERE type = 'event')                                    AS events,
    (array_agg(path ORDER BY ts) FILTER (WHERE type = 'pageview'))[1]         AS entry_path,
    (array_agg(path ORDER BY ts DESC) FILTER (WHERE type = 'pageview'))[1]    AS exit_path,
    (array_agg(referrer_host ORDER BY ts) FILTER (WHERE type = 'pageview'))[1] AS referrer_host,
    (array_agg(utm_source ORDER BY ts) FILTER (WHERE type = 'pageview'))[1]   AS utm_source,
    (array_agg(utm_medium ORDER BY ts) FILTER (WHERE type = 'pageview'))[1]   AS utm_medium,
    (array_agg(utm_campaign ORDER BY ts) FILTER (WHERE type = 'pageview'))[1] AS utm_campaign,
    (array_agg(device ORDER BY ts))[1]                                        AS device,
    (array_agg(browser ORDER BY ts))[1]                                       AS browser,
    (array_agg(os ORDER BY ts))[1]                                            AS os,
    (array_agg(country ORDER BY ts))[1]                                       AS country,
    (coalesce(sum(engaged_ms), 0) >= 10000
        OR count(*) FILTER (WHERE type = 'pageview') >= 2)                    AS engaged
FROM snout.events
GROUP BY site, hostname, session_id;

-- GA's report-snapshot row: users, sessions, views, engaged sessions, bounce
-- rate and average engagement time per active user.
CREATE OR REPLACE VIEW snout.daily AS
SELECT
    (started AT TIME ZONE 'UTC')::date                                         AS day,
    site,
    hostname,
    count(DISTINCT visitor_hash)                                               AS users,
    count(*)                                                                   AS sessions,
    sum(pageviews)                                                             AS pageviews,
    count(*) FILTER (WHERE engaged)                                            AS engaged_sessions,
    round(1 - count(*) FILTER (WHERE engaged)::numeric / nullif(count(*), 0), 4) AS bounce_rate,
    round(sum(engaged_ms)::numeric / nullif(count(DISTINCT visitor_hash), 0) / 1000, 1) AS avg_engagement_s
FROM snout.sessions
GROUP BY 1, 2, 3;

-- GA's "Pages and screens": views, users and average engagement time per view.
CREATE OR REPLACE VIEW snout.daily_pages AS
SELECT
    (ts AT TIME ZONE 'UTC')::date                                              AS day,
    site,
    hostname,
    path,
    count(*) FILTER (WHERE type = 'pageview')                                  AS views,
    count(DISTINCT visitor_hash)                                               AS users,
    round(coalesce(sum(engaged_ms), 0)::numeric
        / nullif(count(*) FILTER (WHERE type = 'pageview'), 0) / 1000, 1)      AS avg_engagement_s
FROM snout.events
WHERE type IN ('pageview', 'engagement')
GROUP BY 1, 2, 3, 4;

CREATE OR REPLACE VIEW snout.daily_entry_pages AS
SELECT (started AT TIME ZONE 'UTC')::date AS day, site, hostname, entry_path AS path,
       count(*) AS sessions, count(DISTINCT visitor_hash) AS users
FROM snout.sessions
WHERE entry_path IS NOT NULL
GROUP BY 1, 2, 3, 4;

CREATE OR REPLACE VIEW snout.daily_exit_pages AS
SELECT (started AT TIME ZONE 'UTC')::date AS day, site, hostname, exit_path AS path,
       count(*) AS sessions, count(DISTINCT visitor_hash) AS users
FROM snout.sessions
WHERE exit_path IS NOT NULL
GROUP BY 1, 2, 3, 4;

-- GA's "Traffic acquisition" by session source / medium. A UTM source wins
-- over the referrer, and a session with neither is (direct).
CREATE OR REPLACE VIEW snout.daily_sources AS
SELECT (started AT TIME ZONE 'UTC')::date                      AS day, site, hostname,
       coalesce(utm_source, referrer_host, '(direct)')         AS source,
       coalesce(utm_medium, CASE WHEN utm_source IS NOT NULL THEN '(not set)'
                                 WHEN referrer_host IS NOT NULL THEN 'referral'
                                 ELSE '(none)' END)            AS medium,
       count(*)                                                AS sessions,
       count(DISTINCT visitor_hash)                            AS users,
       count(*) FILTER (WHERE engaged)                         AS engaged_sessions
FROM snout.sessions
GROUP BY 1, 2, 3, 4, 5;

CREATE OR REPLACE VIEW snout.daily_campaigns AS
SELECT (started AT TIME ZONE 'UTC')::date AS day, site, hostname,
       utm_source AS source, utm_medium AS medium, utm_campaign AS campaign,
       count(*) AS sessions, count(DISTINCT visitor_hash) AS users,
       count(*) FILTER (WHERE engaged) AS engaged_sessions
FROM snout.sessions
WHERE utm_campaign IS NOT NULL
GROUP BY 1, 2, 3, 4, 5, 6;

-- GA's "Tech details": device category, browser, operating system.
CREATE OR REPLACE VIEW snout.daily_tech AS
SELECT (started AT TIME ZONE 'UTC')::date AS day, site, hostname,
       coalesce(device, '(unknown)') AS device, coalesce(browser, '(unknown)') AS browser,
       coalesce(os, '(unknown)') AS os,
       count(*) AS sessions, count(DISTINCT visitor_hash) AS users
FROM snout.sessions
GROUP BY 1, 2, 3, 4, 5, 6;

CREATE OR REPLACE VIEW snout.daily_events AS
SELECT (ts AT TIME ZONE 'UTC')::date AS day, site, hostname, event_name AS name,
       count(*) AS events, count(DISTINCT visitor_hash) AS users
FROM snout.events
WHERE type = 'event'
GROUP BY 1, 2, 3, 4;
