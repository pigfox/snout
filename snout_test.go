package snout

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	chromeUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36"
	testIP   = "203.0.113.77"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time  { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) set(t time.Time) { c.mu.Lock(); c.t = t; c.mu.Unlock() }

func newTracker(t *testing.T, db *fakeDB, cfg Config) (*Tracker, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	cfg.Site = "example"
	cfg.Now = c.now
	if cfg.ClientIP == nil {
		cfg.ClientIP = func(*http.Request) string { return testIP }
	}
	tr, err := New(db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tr.Close(context.Background()) })
	return tr, c
}

func post(tr http.Handler, body, ua string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "https://example.com/_s", strings.NewReader(body))
	r.Host = "example.com:443"
	r.Header.Set("User-Agent", ua)
	w := httptest.NewRecorder()
	tr.ServeHTTP(w, r)
	return w
}

func flushed(t *testing.T, tr *Tracker, db *fakeDB) [][]any {
	t.Helper()
	if err := tr.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	return db.insertedArgs()
}

func TestNewRequiresDBAndSite(t *testing.T) {
	if _, err := New(nil, Config{Site: "x"}); err == nil {
		t.Error("nil DB accepted")
	}
	if _, err := New(newFakeDB(), Config{Site: "  "}); err == nil {
		t.Error("blank site accepted")
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	tr, err := New(newFakeDB(), Config{Site: "x"})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close(context.Background())
	if tr.maxBody != DefaultMaxBody || cap(tr.writer.ch) != DefaultBufferSize ||
		tr.writer.batch != DefaultBatchSize || tr.writer.interval != DefaultFlushInterval ||
		tr.writer.flushTimeout != DefaultFlushTimeout || tr.limiter.limit != DefaultRateLimit ||
		tr.sessions.max != DefaultSessionCap || tr.hosts != nil {
		t.Error("defaults not applied")
	}
	tr.onError(errors.New("default OnError must be a safe no-op"))
	if tr.now().IsZero() {
		t.Error("default clock")
	}
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "198.51.100.1:5555"
	if got := tr.clientIP(r); got != "198.51.100.1" {
		t.Errorf("default ClientIP = %q", got)
	}
}

func TestPageviewIsStoredWithDerivedFields(t *testing.T) {
	db := newFakeDB()
	tr, _ := newTracker(t, db, Config{CountryHeader: "X-Country"})
	r := httptest.NewRequest(http.MethodPost, "/_s", strings.NewReader(
		`{"t":"pageview","p":"/pricing","r":"news.example.org","w":1280,"ti":"Pricing"}`))
	r.Host = "Example.com"
	r.Header.Set("User-Agent", chromeUA)
	r.Header.Set("X-Country", " de ")
	w := httptest.NewRecorder()
	tr.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("code %d", w.Code)
	}
	rows := flushed(t, tr, db)
	if len(rows) != 1 {
		t.Fatalf("got %d inserts", len(rows))
	}
	a := rows[0]
	want := map[int]any{0: "example", 1: "example.com", 5: "pageview", 6: "/pricing", 7: "Pricing",
		8: "news.example.org", 14: "desktop", 15: "Chrome", 16: "macOS", 17: "1024-1439", 18: "DE"}
	for i, v := range want {
		if a[i] != v {
			t.Errorf("%s = %v, want %v", eventColumns[i], a[i], v)
		}
	}
	if len(a[3].(string)) != 64 || len(a[4].(string)) != 32 {
		t.Errorf("hash/session shape: %v %v", a[3], a[4])
	}
	if s := tr.Stats(); s.Accepted != 1 || s.Written != 1 {
		t.Errorf("stats %+v", s)
	}
}

// THE RAW IP NEVER REACHES A ROW. Asserted over every value handed to the
// database, not over the Event struct, so a column added later cannot carry
// it unnoticed.
func TestRawIPAbsentFromEveryStoredValue(t *testing.T) {
	db := newFakeDB()
	tr, _ := newTracker(t, db, Config{})
	post(tr, `{"t":"pageview","p":"/a?x=1","ti":"t"}`, chromeUA)
	post(tr, `{"t":"event","p":"/a","n":"signup","pr":{"plan":"pro"}}`, chromeUA)
	post(tr, `{"t":"engagement","p":"/a","e":1500}`, chromeUA)
	rows := flushed(t, tr, db)
	if len(rows) == 0 {
		t.Fatal("nothing written; the absence below would be vacuous")
	}
	for _, col := range ipColumns(rows, testIP) {
		t.Errorf("column %s carries the raw IP", col)
	}
}

// ipColumns names every column whose stored value contains ip.
func ipColumns(rows [][]any, ip string) []string {
	var out []string
	for _, row := range rows {
		for i, v := range row {
			if strings.Contains(fmt.Sprint(v), ip) {
				out = append(out, eventColumns[i%len(eventColumns)])
			}
		}
	}
	return out
}

func TestIPColumnsIsSharp(t *testing.T) {
	if got := ipColumns([][]any{{"x", "via " + testIP}}, testIP); len(got) != 1 || got[0] != "hostname" {
		t.Errorf("a planted IP was not detected: %v", got)
	}
}

func TestEventAndEngagementPayloads(t *testing.T) {
	db := newFakeDB()
	tr, _ := newTracker(t, db, Config{})
	post(tr, `{"t":"event","p":"/a","n":" signup ","pr":{"plan":"pro"}}`, chromeUA)
	post(tr, `{"t":"engagement","p":"/a","e":99999999999}`, chromeUA)
	rows := flushed(t, tr, db)
	all := append([]any{}, rows[0]...)
	if len(rows) > 1 {
		all = append(all, rows[1]...)
	}
	n := len(eventColumns)
	if all[20] != "signup" || all[21] != `{"plan":"pro"}` {
		t.Errorf("event: %v %v", all[20], all[21])
	}
	if all[n+19] != maxEngaged {
		t.Errorf("engaged clamp: %v", all[n+19])
	}
}

func TestCollectorRefusals(t *testing.T) {
	db := newFakeDB()
	tr, _ := newTracker(t, db, Config{Hosts: []string{"Example.com"}, MaxBody: 64})

	r := httptest.NewRequest(http.MethodGet, "/_s", nil)
	w := httptest.NewRecorder()
	tr.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != http.MethodPost {
		t.Errorf("GET: %d", w.Code)
	}

	if w := post(tr, `{"t":"pageview","p":"/"}`, "Googlebot/2.1"); w.Code != http.StatusNoContent {
		t.Errorf("bot: %d", w.Code)
	}
	if w := post(tr, `{"t":"pageview","p":"/"}`, chromeUA+strings.Repeat(" ", 1)); w.Code != http.StatusNoContent {
		t.Errorf("ok: %d", w.Code)
	}
	if w := post(tr, strings.Repeat("x", 65), chromeUA); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("body cap: %d", w.Code)
	}
	if w := post(tr, `{"t":"nope","p":"/"}`, chromeUA); w.Code != http.StatusBadRequest {
		t.Errorf("invalid: %d", w.Code)
	}

	foreign := httptest.NewRequest(http.MethodPost, "/_s", strings.NewReader(`{"t":"pageview","p":"/"}`))
	foreign.Host = "evil.example"
	foreign.Header.Set("User-Agent", chromeUA)
	w = httptest.NewRecorder()
	tr.ServeHTTP(w, foreign)
	if w.Code != http.StatusBadRequest {
		t.Errorf("foreign host: %d", w.Code)
	}

	s := tr.Stats()
	if s.Bots != 1 || s.TooLarge != 1 || s.Invalid != 2 || s.Accepted != 1 {
		t.Errorf("stats %+v", s)
	}
	if rows := flushed(t, tr, db); len(rows) != 1 {
		t.Errorf("bots must not be stored: %d inserts", len(rows))
	}
}

func TestNoClientIPIsRefused(t *testing.T) {
	db := newFakeDB()
	tr, _ := newTracker(t, db, Config{ClientIP: func(*http.Request) string { return "" }})
	if w := post(tr, `{"t":"pageview","p":"/"}`, chromeUA); w.Code != http.StatusBadRequest {
		t.Errorf("code %d", w.Code)
	}
}

func TestRateLimit(t *testing.T) {
	db := newFakeDB()
	tr, c := newTracker(t, db, Config{RateLimit: 2})
	for i, want := range []int{204, 204, 429} {
		if w := post(tr, `{"t":"pageview","p":"/"}`, chromeUA); w.Code != want {
			t.Errorf("request %d: %d, want %d", i, w.Code, want)
		}
	}
	c.set(c.now().Add(rateWindow))
	if w := post(tr, `{"t":"pageview","p":"/"}`, chromeUA); w.Code != 204 {
		t.Errorf("after the window: %d", w.Code)
	}
	if tr.Stats().RateLimited != 1 {
		t.Error("rate-limited not counted")
	}
}

func TestLimiterResetsWhenClockGoesBackwards(t *testing.T) {
	l := newLimiter(1, time.Minute)
	now := time.Unix(1000, 0)
	l.allow("a", now)
	if l.allow("a", now) {
		t.Fatal("second request in window allowed")
	}
	if !l.allow("a", now.Add(-time.Second)) {
		t.Error("a backwards clock must start a new window")
	}
}

func TestSaltErrorDropsAndReports(t *testing.T) {
	var got error
	db := newFakeDB()
	db.failQuery = true
	tr, _ := newTracker(t, db, Config{OnError: func(err error) { got = err }})
	if w := post(tr, `{"t":"pageview","p":"/"}`, chromeUA); w.Code != http.StatusNoContent {
		t.Errorf("code %d", w.Code)
	}
	if got == nil || tr.Stats().Dropped != 1 {
		t.Error("salt failure not reported/counted")
	}
}

func TestCountryHeaderValidation(t *testing.T) {
	tr := &Tracker{countryHeader: "X-C"}
	for in, want := range map[string]string{"us": "US", "USA": "", "1A": "", "A1": "", "": "", "{a": ""} {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set("X-C", in)
		if got := tr.country(r); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if (&Tracker{}).country(httptest.NewRequest(http.MethodPost, "/", nil)) != "" {
		t.Error("no header configured must store nothing")
	}
}
