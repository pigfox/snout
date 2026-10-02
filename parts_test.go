package snout

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- payload ----

// UTM-ONLY QUERY RETENTION: whatever query string arrives on the path, only
// the five utm_ keys survive, and they survive in their own columns.
func TestOnlyUTMKeysSurviveTheQueryString(t *testing.T) {
	e, ok := parsePayload([]byte(`{"t":"pageview","p":"/x?utm_source=news&token=SECRET&utm_campaign=fall#frag"}`), "h")
	if !ok {
		t.Fatal("rejected")
	}
	if e.Path != "/x" || e.UTMSource != "news" || e.UTMCampaign != "fall" {
		t.Errorf("%+v", e)
	}
	if strings.Contains(e.Path+e.Title+e.UTMSource+e.UTMMedium+e.UTMCampaign+e.UTMTerm+e.UTMContent, "SECRET") {
		t.Error("a non-UTM query value survived")
	}
	// The script's own utm object wins over the path's.
	e, _ = parsePayload([]byte(`{"t":"pageview","p":"/x?utm_source=a","u":{"source":"b","medium":"m","term":"t","content":"c","other":"z"}}`), "h")
	if e.UTMSource != "b" || e.UTMMedium != "m" || e.UTMTerm != "t" || e.UTMContent != "c" {
		t.Errorf("%+v", e)
	}
	// A malformed query yields no UTMs rather than a rejection.
	e, ok = parsePayload([]byte(`{"t":"pageview","p":"/x?%zz"}`), "h")
	if !ok || e.UTMSource != "" {
		t.Errorf("malformed query: %v %+v", ok, e)
	}
}

func TestParsePayloadRejects(t *testing.T) {
	for _, body := range []string{
		`not json`,
		`{"t":"click","p":"/"}`,
		`{"t":"pageview","p":"relative"}`,
		`{"t":"pageview","p":"#/x"}`,
		`{"t":"event","p":"/","n":"  "}`,
		`{"t":"event","p":"/","n":"x","pr":[1]}`,
		`{"t":"event","p":"/","n":"x","pr":{"k":"` + strings.Repeat("v", maxProps) + `"}}`,
	} {
		if _, ok := parsePayload([]byte(body), "h"); ok {
			t.Errorf("accepted %s", body)
		}
	}
	if e, ok := parsePayload([]byte(`{"t":"event","p":"/","n":"x","pr":null}`), "h"); !ok || e.Props != "" {
		t.Error("null props must be accepted as none")
	}
	if e, _ := parsePayload([]byte(`{"t":"engagement","p":"/","e":-5}`), "h"); e.EngagedMS != 0 {
		t.Error("negative engaged time must clamp to zero")
	}
}

func TestReferrerHost(t *testing.T) {
	for in, want := range map[string]string{
		"":                               "",
		"news.example.org":               "news.example.org",
		"https://News.Example.org/a?q=1": "news.example.org",
		"https://self.example/x":         "",
		"https://":                       "",
		"http://[::1":                    "",
		strings.Repeat("a", 260) + ".io": "",
	} {
		if got := referrerHost(in, "self.example"); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestTruncateKeepsUTF8Whole(t *testing.T) {
	if got := truncate("aé", 2); got != "a" {
		t.Errorf("%q", got)
	}
	if got := truncate("abc", 5); got != "abc" {
		t.Errorf("%q", got)
	}
}

func TestHostOfAndRemoteIP(t *testing.T) {
	if hostOf("Example.COM:8080") != "example.com" || hostOf("example.com") != "example.com" {
		t.Error("hostOf")
	}
	if remoteIP(&http.Request{RemoteAddr: "198.51.100.9"}) != "198.51.100.9" {
		t.Error("RemoteAddr without a port must be used as is")
	}
	if orDuration(time.Second, time.Hour) != time.Second {
		t.Error("orDuration must keep a set value")
	}
}

// ---- user agent ----

func TestIsBot(t *testing.T) {
	for _, ua := range []string{"", "  ", "Googlebot/2.1", "Mozilla/5.0 HeadlessChrome/128", "curl/8.0", "Go-http-client/1.1", "Render/1.0"} {
		if !IsBot(ua) {
			t.Errorf("%q not a bot", ua)
		}
	}
	if IsBot(chromeUA) {
		t.Error("desktop Chrome flagged as a bot")
	}
}

func TestDeviceBrowserOS(t *testing.T) {
	cases := []struct{ ua, dev, br, os string }{
		{chromeUA, "desktop", "Chrome", "macOS"},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) Version/17.0 Mobile Safari/604.1", "mobile", "Safari", "iOS"},
		{"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) CriOS/128.0 Mobile Safari/604.1", "tablet", "Chrome", "iOS"},
		{"Mozilla/5.0 (Linux; Android 14; SM-X) AppleWebKit/537.36 Chrome/128 Safari/537.36", "tablet", "Chrome", "Android"},
		{"Mozilla/5.0 (Linux; Android 14) SamsungBrowser/25.0 Chrome/121 Mobile Safari/537.36", "mobile", "Samsung Internet", "Android"},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/128 Safari/537.36 Edg/128.0", "desktop", "Edge", "Windows"},
		{"Mozilla/5.0 (Windows NT 10.0) Chrome/128 Safari/537.36 OPR/112", "desktop", "Opera", "Windows"},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0", "desktop", "Firefox", "Linux"},
		{"Mozilla/5.0 (X11; CrOS x86_64 15000) Chrome/128 Safari/537.36", "desktop", "Chrome", "ChromeOS"},
		{"Mozilla/5.0 (Tablet; rv:1) Gecko Firefox/1", "tablet", "Firefox", "Other"},
		{"SomethingElse/1.0", "desktop", "Other", "Other"},
	}
	for _, c := range cases {
		if d, b, o := deviceClass(c.ua), browserName(c.ua), osName(c.ua); d != c.dev || b != c.br || o != c.os {
			t.Errorf("%q -> %s/%s/%s, want %s/%s/%s", c.ua, d, b, o, c.dev, c.br, c.os)
		}
	}
}

func TestScreenBucket(t *testing.T) {
	for w, want := range map[int]string{0: "", -1: "", 375: "<576", 600: "576-767", 800: "768-1023", 1280: "1024-1439", 2560: "1440+"} {
		if got := screenBucket(w); got != want {
			t.Errorf("%d -> %q, want %q", w, got, want)
		}
	}
}

// ---- hash and salt ----

func TestHashStableWithinADayAndChangesAcrossRotation(t *testing.T) {
	db := newFakeDB()
	s := &salter{db: db, rand: strings.NewReader(strings.Repeat("a", saltBytes) + strings.Repeat("b", saltBytes))}
	day1 := time.Date(2026, 10, 2, 0, 0, 1, 0, time.UTC)

	s1, err := s.get(context.Background(), day1)
	if err != nil {
		t.Fatal(err)
	}
	s2, _ := s.get(context.Background(), day1.Add(23*time.Hour))
	if visitorHash(s1, "site", "ip", "ua") != visitorHash(s2, "site", "ip", "ua") {
		t.Error("hash changed within one UTC day")
	}

	s3, err := s.get(context.Background(), day1.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if visitorHash(s1, "site", "ip", "ua") == visitorHash(s3, "site", "ip", "ua") {
		t.Error("hash survived salt rotation")
	}
	if _, ok := db.salts["2026-10-02"]; ok || len(db.salts) != 1 {
		t.Errorf("previous day's salt not deleted: %v", db.salts)
	}
}

// A second process (or a restart) that offers a different salt for the same
// day adopts the stored one, so the day is not split.
func TestSaltIsSharedThroughTheTable(t *testing.T) {
	db := newFakeDB()
	day := time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC)
	a := &salter{db: db, rand: strings.NewReader(strings.Repeat("a", saltBytes))}
	b := &salter{db: db, rand: strings.NewReader(strings.Repeat("b", saltBytes))}
	sa, _ := a.get(context.Background(), day)
	sb, _ := b.get(context.Background(), day)
	if string(sa) != string(sb) {
		t.Error("two processes hashed the same day with different salts")
	}
}

func TestSaltErrors(t *testing.T) {
	day := time.Now()
	if _, err := (&salter{db: newFakeDB(), rand: errReader{}}).get(context.Background(), day); err == nil {
		t.Error("entropy failure ignored")
	}
	for _, fail := range []string{"INSERT INTO snout.salt", "DELETE FROM snout.salt"} {
		db := newFakeDB()
		db.failOn = fail
		if _, err := (&salter{db: db, rand: strings.NewReader(strings.Repeat("a", saltBytes))}).get(context.Background(), day); err == nil {
			t.Errorf("%s failure ignored", fail)
		}
	}
}

func TestVisitorHashSeparatesFields(t *testing.T) {
	if visitorHash(nil, "ab", "c", "") == visitorHash(nil, "a", "bc", "") {
		t.Error("field boundary is ambiguous")
	}
}

// ---- sessions ----

func TestSessionSplitsAtThirtyMinutes(t *testing.T) {
	s := newSessions(10)
	t0 := time.Unix(10_000, 0)
	id := s.assign("v", t0)
	if s.assign("v", t0.Add(29*time.Minute)) != id {
		t.Error("29 minutes of inactivity split the session")
	}
	// The window slides: 29 minutes after the LAST event still continues.
	if s.assign("v", t0.Add(58*time.Minute)) != id {
		t.Error("session did not extend with activity")
	}
	// An out-of-order earlier event joins without moving last-seen back.
	if s.assign("v", t0.Add(57*time.Minute)) != id {
		t.Error("late event split the session")
	}
	if s.assign("v", t0.Add(88*time.Minute)) == id {
		t.Error("30 minutes of inactivity did not start a new session")
	}
	if s.assign("w", t0) == id {
		t.Error("two visitors shared a session")
	}
}

func TestSessionCapSweepsThenDegrades(t *testing.T) {
	s := newSessions(1)
	t0 := time.Unix(10_000, 0)
	s.assign("a", t0)
	// Full, nothing expired: b gets an id but is not tracked.
	idB := s.assign("b", t0)
	if s.assign("b", t0.Add(time.Minute)) == idB {
		t.Error("an untracked session was continued")
	}
	// Full, a has expired: the sweep makes room.
	s.assign("c", t0.Add(SessionTimeout))
	if _, ok := s.open["c"]; !ok {
		t.Error("sweep did not make room")
	}
}

// ---- writer ----

func newWriter(db DB, buf, batch int, interval time.Duration, onErr func(error)) *writer {
	if onErr == nil {
		onErr = func(error) {}
	}
	return &writer{
		db: db, ch: make(chan Event, buf), batch: batch, interval: interval,
		flushTimeout: time.Second, onError: onErr,
		dropped: new(atomic.Uint64), written: new(atomic.Uint64), flushErrors: new(atomic.Uint64),
		done: make(chan struct{}), stopped: make(chan struct{}),
	}
}

func TestBatchFlushOnSize(t *testing.T) {
	db := newFakeDB()
	w := newWriter(db, 10, 3, time.Hour, nil)
	go w.run()
	for i := 0; i < 3; i++ {
		w.enqueue(Event{Type: "pageview", Path: "/"})
	}
	select {
	case n := <-db.inserted:
		if n != 3 {
			t.Errorf("batch of %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("full batch was not flushed")
	}
	_ = w.close(context.Background())
}

func TestBatchFlushOnInterval(t *testing.T) {
	db := newFakeDB()
	w := newWriter(db, 10, 100, 20*time.Millisecond, nil)
	go w.run()
	w.enqueue(Event{Type: "pageview", Path: "/"})
	select {
	case n := <-db.inserted:
		if n != 1 {
			t.Errorf("batch of %d", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("interval flush never happened")
	}
	_ = w.close(context.Background())
}

func TestShutdownFlushesEverything(t *testing.T) {
	db := newFakeDB()
	w := newWriter(db, 10, 2, time.Hour, nil)
	// Queue before the loop starts, so close drains a backlog bigger than
	// one batch: two full batches and a remainder.
	for i := 0; i < 5; i++ {
		w.enqueue(Event{Type: "pageview", Path: "/"})
	}
	go w.run()
	if err := w.close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.close(context.Background()); err != nil {
		t.Fatal("second close must be harmless")
	}
	if w.written.Load() != 5 {
		t.Errorf("written %d of 5", w.written.Load())
	}
}

func TestCloseHonorsContext(t *testing.T) {
	w := newWriter(newFakeDB(), 1, 1, time.Hour, nil) // loop never started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.close(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("err %v", err)
	}
}

func TestBufferFullDropsWithoutBlocking(t *testing.T) {
	w := newWriter(newFakeDB(), 1, 10, time.Hour, nil) // loop not running
	w.enqueue(Event{})
	done := make(chan struct{})
	go func() { w.enqueue(Event{}); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("enqueue blocked on a full buffer")
	}
	if w.dropped.Load() != 1 {
		t.Errorf("dropped %d", w.dropped.Load())
	}
}

func TestFailedBatchIsCountedAndReported(t *testing.T) {
	db := newFakeDB()
	db.insertErr = errors.New("db down")
	var got error
	w := newWriter(db, 10, 10, time.Hour, func(err error) { got = err })
	w.enqueue(Event{})
	w.enqueue(Event{})
	go w.run()
	_ = w.close(context.Background())
	if got == nil || w.flushErrors.Load() != 1 || w.dropped.Load() != 2 {
		t.Errorf("err %v flushErrors %d dropped %d", got, w.flushErrors.Load(), w.dropped.Load())
	}
}

// ---- store ----

func TestInsertSQLShape(t *testing.T) {
	sql := insertSQL(2)
	n := len(eventColumns)
	if !strings.Contains(sql, "($1, $2") || !strings.Contains(sql, "), ($23, ") ||
		strings.Count(sql, "::jsonb") != 2 || !strings.HasSuffix(sql, "$44::jsonb)") || n != 22 {
		t.Errorf("%s", sql)
	}
	if err := insertEvents(context.Background(), newFakeDB(), nil); err != nil {
		t.Error("empty batch must be a no-op")
	}
}

func TestEventArgsNullsEmptyValues(t *testing.T) {
	a := eventArgs(Event{Site: "s", Hostname: "h", Type: "pageview", Path: "/"})
	if len(a) != len(eventColumns) {
		t.Fatalf("%d args for %d columns", len(a), len(eventColumns))
	}
	for i := 7; i < len(a); i++ {
		if a[i] != nil {
			t.Errorf("%s = %v, want NULL", eventColumns[i], a[i])
		}
	}
}

func TestPrune(t *testing.T) {
	db := newFakeDB()
	if err := Prune(context.Background(), db, 48*time.Hour); err != nil {
		t.Fatal(err)
	}
	c := db.execs[0]
	if !strings.HasPrefix(c.sql, "DELETE FROM snout.events WHERE ts < now()") || c.args[0] != float64(172800) {
		t.Errorf("%s %v", c.sql, c.args)
	}
}

func TestSchemaAndScriptAreEmbedded(t *testing.T) {
	for _, want := range []string{"CREATE SCHEMA IF NOT EXISTS snout", "CREATE TABLE IF NOT EXISTS snout.events", "snout.daily_sources"} {
		if !strings.Contains(Schema, want) {
			t.Errorf("Schema missing %q", want)
		}
	}
	for _, col := range eventColumns {
		if !strings.Contains(Schema, "\n    "+col+" ") {
			t.Errorf("insert column %q not in the events table", col)
		}
	}
	js := string(Script)
	for _, want := range []string{"sendBeacon", "visibilitychange", "prerendering", "data-endpoint", "utm_"} {
		if !strings.Contains(js, want) {
			t.Errorf("Script missing %q", want)
		}
	}
	for _, banned := range []string{"document.cookie", "localStorage", "sessionStorage", "indexedDB", "location.search }"} {
		if strings.Contains(js, banned) {
			t.Errorf("Script must not use %q", banned)
		}
	}
}
