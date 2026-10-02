package snout

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// DefaultPath is a suggested collector mount path. It avoids "analytics",
// "collect", "track" and similar words because ad-blocking filter lists match
// on them; any neutral path works, and the script reads it from its tag.
const DefaultPath = "/_s"

// Defaults for the zero values in Config.
const (
	DefaultBufferSize    = 4096
	DefaultBatchSize     = 200
	DefaultFlushInterval = 5 * time.Second
	DefaultFlushTimeout  = 10 * time.Second
	DefaultMaxBody       = 4 << 10
	DefaultRateLimit     = 120
	DefaultSessionCap    = 100_000
	rateWindow           = time.Minute
)

// Field limits. Anything longer is truncated, except props, which are refused.
const (
	maxPath     = 512
	maxTitle    = 300
	maxName     = 64
	maxUTM      = 200
	maxHost     = 253
	maxProps    = 1024
	maxEngaged  = int64(6 * time.Hour / time.Millisecond)
	countryLen  = 2
	typePage    = "pageview"
	typeEngaged = "engagement"
	typeEvent   = "event"
)

// Config configures a Tracker. Site is required; every other field has a
// default.
type Config struct {
	// Site names the property; it is part of every visitor hash, so two sites
	// sharing a database never share a visitor.
	Site string
	// Hosts lists the hostnames (from the request's Host header) whose events
	// are accepted. Empty accepts any. Every row records its hostname, so a
	// dev host sharing the database is separated by filtering on it.
	Hosts []string
	// ClientIP derives the client address. Default: the host part of
	// RemoteAddr. Behind a proxy, pass the function the application already
	// trusts. The value feeds the visitor hash and the rate limiter only.
	ClientIP func(*http.Request) string
	// CountryHeader names a trusted upstream header carrying an ISO 3166-1
	// alpha-2 country code. Empty stores NULL.
	CountryHeader string

	BufferSize    int           // events held in memory before dropping
	BatchSize     int           // events per INSERT
	FlushInterval time.Duration // longest an event waits in the buffer
	FlushTimeout  time.Duration // deadline for one batch INSERT
	MaxBody       int64         // collector request body cap, bytes
	RateLimit     int           // requests per client IP per minute
	SessionCap    int           // open sessions tracked in memory

	// OnError receives write and salt errors. They never carry an IP.
	OnError func(error)
	// Now and Rand exist for tests.
	Now  func() time.Time
	Rand io.Reader
}

// Stats are cumulative counters since New.
type Stats struct {
	Accepted    uint64 // events queued
	Written     uint64 // events written
	Bots        uint64 // requests dropped as automated
	Dropped     uint64 // events lost to a full buffer, a salt error or a failed batch
	FlushErrors uint64 // failed batch INSERTs
	RateLimited uint64
	Invalid     uint64 // malformed payloads, foreign hosts, no client IP
	TooLarge    uint64
}

// Tracker is the collector: an http.Handler that accepts beacons, plus the
// buffered writer behind it. Create it with New and stop it with Close.
type Tracker struct {
	site          string
	hosts         map[string]bool
	clientIP      func(*http.Request) string
	countryHeader string
	maxBody       int64
	now           func() time.Time
	onError       func(error)

	salter   *salter
	sessions *sessions
	limiter  *limiter
	writer   *writer

	accepted, written, bots, dropped, flushErrors, rateLimited, invalid, tooLarge atomic.Uint64
}

// New builds a Tracker writing through db and starts its writer.
func New(db DB, cfg Config) (*Tracker, error) {
	if db == nil {
		return nil, errors.New("snout: nil DB")
	}
	if strings.TrimSpace(cfg.Site) == "" {
		return nil, errors.New("snout: Config.Site is required")
	}
	t := &Tracker{
		site:          cfg.Site,
		clientIP:      cfg.ClientIP,
		countryHeader: cfg.CountryHeader,
		maxBody:       orInt64(cfg.MaxBody, DefaultMaxBody),
		now:           cfg.Now,
		onError:       cfg.OnError,
	}
	if t.clientIP == nil {
		t.clientIP = remoteIP
	}
	if t.now == nil {
		t.now = time.Now
	}
	if t.onError == nil {
		t.onError = func(error) {}
	}
	if len(cfg.Hosts) > 0 {
		t.hosts = make(map[string]bool, len(cfg.Hosts))
		for _, h := range cfg.Hosts {
			t.hosts[strings.ToLower(h)] = true
		}
	}
	r := cfg.Rand
	if r == nil {
		r = rand.Reader
	}
	t.salter = &salter{db: db, rand: r}
	t.sessions = newSessions(orInt(cfg.SessionCap, DefaultSessionCap))
	t.limiter = newLimiter(orInt(cfg.RateLimit, DefaultRateLimit), rateWindow)
	t.writer = &writer{
		db:           db,
		ch:           make(chan Event, orInt(cfg.BufferSize, DefaultBufferSize)),
		batch:        orInt(cfg.BatchSize, DefaultBatchSize),
		interval:     orDuration(cfg.FlushInterval, DefaultFlushInterval),
		flushTimeout: orDuration(cfg.FlushTimeout, DefaultFlushTimeout),
		onError:      t.onError,
		dropped:      &t.dropped,
		written:      &t.written,
		flushErrors:  &t.flushErrors,
		done:         make(chan struct{}),
		stopped:      make(chan struct{}),
	}
	go t.writer.run()
	return t, nil
}

// Close writes every buffered event and stops the writer. It returns ctx's
// error if the final flush outlives ctx.
func (t *Tracker) Close(ctx context.Context) error {
	return t.writer.close(ctx)
}

// Stats returns the counters.
func (t *Tracker) Stats() Stats {
	return Stats{
		Accepted:    t.accepted.Load(),
		Written:     t.written.Load(),
		Bots:        t.bots.Load(),
		Dropped:     t.dropped.Load(),
		FlushErrors: t.flushErrors.Load(),
		RateLimited: t.rateLimited.Load(),
		Invalid:     t.invalid.Load(),
		TooLarge:    t.tooLarge.Load(),
	}
}

// ServeHTTP is the collector. It answers 204 for anything it accepted or
// deliberately ignored (bots), so a beacon never retries; 405, 413, 429 and
// 400 are for callers that are not the script.
func (t *Tracker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Cache-Control", "no-store")

	ip := t.clientIP(r)
	if ip == "" {
		t.invalid.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	now := t.now()
	if !t.limiter.allow(ip, now) {
		t.rateLimited.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	ua := r.UserAgent()
	if IsBot(ua) {
		t.bots.Add(1)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	host := hostOf(r.Host)
	if t.hosts != nil && !t.hosts[host] {
		t.invalid.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, t.maxBody))
	if err != nil {
		t.tooLarge.Add(1)
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	e, ok := parsePayload(body, host)
	if !ok {
		t.invalid.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	salt, err := t.salter.get(r.Context(), now)
	if err != nil {
		t.dropped.Add(1)
		t.onError(err)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	e.Site = t.site
	e.Hostname = host
	e.TS = now.UTC()
	e.VisitorHash = visitorHash(salt, t.site, ip, ua)
	e.SessionID = t.sessions.assign(e.VisitorHash, now)
	e.Device = deviceClass(ua)
	e.Browser = browserName(ua)
	e.OS = osName(ua)
	e.Country = t.country(r)

	t.accepted.Add(1)
	t.writer.enqueue(e)
	w.WriteHeader(http.StatusNoContent)
}

func (t *Tracker) country(r *http.Request) string {
	if t.countryHeader == "" {
		return ""
	}
	c := strings.ToUpper(strings.TrimSpace(r.Header.Get(t.countryHeader)))
	if len(c) != countryLen || c[0] < 'A' || c[0] > 'Z' || c[1] < 'A' || c[1] > 'Z' {
		return ""
	}
	return c
}

// payload is what the script sends. Keys are short because every byte is in
// every beacon.
type payload struct {
	T  string            `json:"t"`
	P  string            `json:"p"`
	R  string            `json:"r"`
	U  map[string]string `json:"u"`
	W  int               `json:"w"`
	Ti string            `json:"ti"`
	E  int64             `json:"e"`
	N  string            `json:"n"`
	Pr json.RawMessage   `json:"pr"`
}

var utmKeys = []string{"source", "medium", "campaign", "term", "content"}

// parsePayload validates a beacon into an Event. The path keeps no query
// string: UTM parameters are lifted out of it into their own columns and
// everything else in it is discarded, so a token in a URL never reaches a row.
func parsePayload(body []byte, host string) (Event, bool) {
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return Event{}, false
	}
	switch p.T {
	case typePage, typeEngaged, typeEvent:
	default:
		return Event{}, false
	}

	rawPath, rawQuery, _ := strings.Cut(p.P, "?")
	rawPath, _, _ = strings.Cut(rawPath, "#")
	if !strings.HasPrefix(rawPath, "/") {
		return Event{}, false
	}
	rawQuery, _, _ = strings.Cut(rawQuery, "#")

	utm := make(map[string]string, len(utmKeys))
	if q, err := url.ParseQuery(rawQuery); err == nil {
		for _, k := range utmKeys {
			utm[k] = q.Get("utm_" + k)
		}
	}
	for _, k := range utmKeys {
		if v := p.U[k]; v != "" {
			utm[k] = v
		}
	}

	e := Event{
		Type:         p.T,
		Path:         truncate(rawPath, maxPath),
		Title:        truncate(strings.TrimSpace(p.Ti), maxTitle),
		ReferrerHost: referrerHost(p.R, host),
		UTMSource:    truncate(utm["source"], maxUTM),
		UTMMedium:    truncate(utm["medium"], maxUTM),
		UTMCampaign:  truncate(utm["campaign"], maxUTM),
		UTMTerm:      truncate(utm["term"], maxUTM),
		UTMContent:   truncate(utm["content"], maxUTM),
		ScreenBucket: screenBucket(p.W),
	}
	if p.T == typeEngaged {
		e.EngagedMS = min(max(p.E, 0), maxEngaged)
	}
	if p.T == typeEvent {
		e.Name = truncate(strings.TrimSpace(p.N), maxName)
		if e.Name == "" {
			return Event{}, false
		}
		props, ok := cleanProps(p.Pr)
		if !ok {
			return Event{}, false
		}
		e.Props = props
	}
	return e, true
}

// cleanProps accepts an absent or null value, or a JSON object of at most
// maxProps bytes.
func cleanProps(raw json.RawMessage) (string, bool) {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "", true
	}
	if len(s) > maxProps || s[0] != '{' {
		return "", false
	}
	return s, true
}

// referrerHost reduces a referrer to its host and drops it when it is the
// site itself, so internal navigation does not read as a traffic source.
func referrerHost(ref, self string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	if !strings.Contains(ref, "://") {
		ref = "https://" + ref
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	if h == "" || h == self || len(h) > maxHost {
		return ""
	}
	return h
}

func hostOf(hostport string) string {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	return strings.ToLower(h)
}

func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

func orInt(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}

func orInt64(v, d int64) int64 {
	if v > 0 {
		return v
	}
	return d
}

func orDuration(v, d time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return d
}
