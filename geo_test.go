package snout

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	berlinIP = "198.51.100.9"
	parisIP  = "2001:db8::7"
	bareIP   = "192.0.2.4" // country and continent only, as in a Country database
)

func fixtureDB(t *testing.T) []byte {
	return buildMMDB(t, 6, map[string]map[string]any{
		"198.51.100.0/24": cityRecord("EU", "DE", "Land Berlin", "Berlin"),
		"2001:db8::/32":   cityRecord("EU", "FR", "Île-de-France", "Paris"),
		"192.0.2.0/24":    cityRecord("NA", "US", "", ""),
	})
}

func writeFile(t *testing.T, path string, b []byte, mod time.Time) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

type errLog struct {
	mu   sync.Mutex
	errs []error
}

func (l *errLog) add(err error) { l.mu.Lock(); l.errs = append(l.errs, err); l.mu.Unlock() }
func (l *errLog) n() int        { l.mu.Lock(); defer l.mu.Unlock(); return len(l.errs) }
func (l *errLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return fmt.Sprint(l.errs)
}

// geoAt builds a geoDB over path without its watch goroutine, so reloads
// happen exactly when the test calls them.
func geoAt(path string, log *errLog) *geoDB {
	g := &geoDB{path: path, onError: log.add}
	g.reload()
	return g
}

func TestGeoLookup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.mmdb")
	writeFile(t, path, fixtureDB(t), time.Unix(1, 0))
	log := &errLog{}
	g := geoAt(path, log)
	for ip, want := range map[string]location{
		berlinIP:              {"EU", "DE", "Land Berlin", "Berlin"},
		"::ffff:198.51.100.9": {"EU", "DE", "Land Berlin", "Berlin"},
		parisIP:               {"EU", "FR", "Île-de-France", "Paris"},
		bareIP:                {"NA", "US", "", ""},
		"203.0.113.1":         {}, // not in the database
		"not an ip":           {},
		"":                    {},
	} {
		if got := g.lookup(ip); got != want {
			t.Errorf("%q -> %+v, want %+v", ip, got, want)
		}
	}
	if log.n() != 0 {
		t.Errorf("errors %v", log.text())
	}
}

// A lookup error's text can carry the IP, so it is swallowed, never reported.
func TestGeoLookupErrorIsNotReported(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.mmdb")
	writeFile(t, path, buildMMDB(t, 4, map[string]map[string]any{
		"198.51.100.0/24": cityRecord("EU", "DE", "Land Berlin", "Berlin"),
	}), time.Unix(1, 0))
	log := &errLog{}
	g := geoAt(path, log)
	if got := g.lookup(berlinIP); got.city != "Berlin" {
		t.Fatalf("IPv4 database not read: %+v", got)
	}
	if got := g.lookup(parisIP); got != (location{}) || log.n() != 0 {
		t.Errorf("IPv6 in an IPv4 database: %+v, errors %v", got, log.text())
	}
	// A record whose types do not fit geoRecord is a decode error: also silent.
	writeFile(t, path, buildMMDB(t, 6, map[string]map[string]any{
		"198.51.100.0/24": {"country": "DE"},
	}), time.Unix(2, 0))
	g.reload()
	if got := g.lookup(berlinIP); got != (location{}) || log.n() != 0 {
		t.Errorf("decode error: %+v, errors %v", got, log.text())
	}
}

func TestGeoValuesAreCleaned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.mmdb")
	writeFile(t, path, buildMMDB(t, 6, map[string]map[string]any{
		"198.51.100.0/24": cityRecord("eu", "de", " Land\nBerlin ", strings.Repeat("ü", 80)),
		"192.0.2.0/24":    cityRecord("ZZ", "XX", "", ""),
	}), time.Unix(1, 0))
	g := geoAt(path, &errLog{})
	got := g.lookup(berlinIP)
	if got.continent != "EU" || got.country != "DE" || got.region != "" || got.city != strings.Repeat("ü", 50) {
		t.Errorf("%+v", got)
	}
	if got := g.lookup(bareIP); got != (location{}) {
		t.Errorf("unknown codes kept: %+v", got)
	}
}

// The file's life: absent at start, then present, unchanged, replaced,
// unreadable, corrupt and deleted. Every failure turns location off and is
// reported once per change of the file, never an error per request.
func TestGeoReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "city.mmdb")
	log := &errLog{}
	g := geoAt(path, log)
	if g.reader.Load() != nil || log.n() != 1 || !strings.Contains(log.text(), "geo database off") {
		t.Fatalf("missing file: reader %v, errors %v", g.reader.Load(), log.text())
	}
	g.reload()
	if log.n() != 1 {
		t.Error("a file still missing was reported again")
	}

	writeFile(t, path, fixtureDB(t), time.Unix(1, 0))
	g.reload()
	if g.lookup(berlinIP).city != "Berlin" {
		t.Fatal("file appearing was not loaded")
	}
	first := g.reader.Load()
	g.reload()
	if g.reader.Load() != first {
		t.Error("unchanged file was read again")
	}

	writeFile(t, path, buildMMDB(t, 6, map[string]map[string]any{
		"198.51.100.0/24": cityRecord("EU", "DE", "Hamburg", "Hamburg"),
	}), time.Unix(2, 0))
	g.reload()
	if g.lookup(berlinIP).city != "Hamburg" {
		t.Error("replaced file was not reloaded")
	}

	writeFile(t, path, []byte("not a database"), time.Unix(3, 0))
	g.reload()
	if g.reader.Load() != nil || g.lookup(berlinIP) != (location{}) || log.n() != 2 {
		t.Errorf("corrupt file: errors %v", log.text())
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	g.reload()
	if g.reader.Load() != nil || log.n() != 3 {
		t.Errorf("unreadable path: errors %v", log.text())
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, fixtureDB(t), time.Unix(4, 0))
	g.reload()
	if g.lookup(berlinIP).city != "Berlin" {
		t.Fatal("not reloaded after recovery")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	g.reload()
	if g.reader.Load() != nil || log.n() != 4 {
		t.Errorf("deleted file: errors %v", log.text())
	}
	if strings.Contains(log.text(), berlinIP) {
		t.Error("an error carries an IP")
	}
}

// Through New: the watcher picks a file up without a restart, and Close stops
// it.
func TestGeoWatcherReloadsWithoutRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.mmdb")
	log := &errLog{}
	tr, _ := newTracker(t, newFakeDB(), Config{GeoDB: path, GeoDBCheck: time.Millisecond, OnError: log.add})
	if tr.geo == nil || tr.geo.reader.Load() != nil || log.n() != 1 {
		t.Fatalf("missing file at start: errors %v", log.text())
	}
	writeFile(t, path, fixtureDB(t), time.Unix(1, 0))
	deadline := time.Now().Add(10 * time.Second)
	for tr.geo.reader.Load() == nil {
		if time.Now().After(deadline) {
			t.Fatal("watcher never loaded the file")
		}
		time.Sleep(time.Millisecond)
	}
	if err := tr.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-tr.geo.stopped:
	default:
		t.Error("watcher still running after Close")
	}
	if err := tr.Close(context.Background()); err != nil {
		t.Error("second Close", err)
	}
}

func TestNewDefaultsGeoDBCheck(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.mmdb")
	writeFile(t, path, fixtureDB(t), time.Unix(1, 0))
	tr, _ := newTracker(t, newFakeDB(), Config{GeoDB: path})
	if tr.geo.lookup(berlinIP).city != "Berlin" {
		t.Error("GeoDB not loaded by New")
	}
}

func TestLocationSources(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.mmdb")
	writeFile(t, path, fixtureDB(t), time.Unix(1, 0))
	g := geoAt(path, &errLog{})
	cloudflare := func(geo *geoDB) *Tracker {
		return &Tracker{
			countryHeader: CloudflareCountryHeader, continentHeader: CloudflareContinentHeader,
			regionHeader: CloudflareRegionHeader, cityHeader: CloudflareCityHeader, geo: geo,
		}
	}
	headersOnly, withDB, dbOnly := cloudflare(nil), cloudflare(g), &Tracker{geo: g}

	req := func(h map[string]string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		for k, v := range h {
			r.Header.Set(k, v)
		}
		return r
	}
	all := map[string]string{"cf-ipcountry": "AT", "cf-ipcontinent": "EU", "cf-region": "Vienna", "cf-ipcity": "Vienna"}
	cases := []struct {
		name string
		tr   *Tracker
		h    map[string]string
		ip   string
		want location
	}{
		{"headers only", headersOnly, all, berlinIP, location{"EU", "AT", "Vienna", "Vienna"}},
		{"headers complete: no lookup needed", withDB, all, berlinIP, location{"EU", "AT", "Vienna", "Vienna"}},
		{"country header only, no database", headersOnly, map[string]string{"CF-IPCountry": "de"}, berlinIP, location{"", "DE", "", ""}},
		{"database fills what agrees", withDB, map[string]string{"CF-IPCountry": "DE"}, berlinIP, location{"EU", "DE", "Land Berlin", "Berlin"}},
		{"header values win over database", withDB, map[string]string{"CF-IPCountry": "DE", "CF-IPCity": "Potsdam"}, berlinIP, location{"EU", "DE", "Land Berlin", "Potsdam"}},
		{"database disagrees: nothing mixed in", withDB, map[string]string{"CF-IPCountry": "AT"}, berlinIP, location{"", "AT", "", ""}},
		{"no country header: database answers", withDB, map[string]string{"CF-IPCity": "Potsdam"}, parisIP, location{"EU", "FR", "Île-de-France", "Paris"}},
		{"unknown country XX: database answers", withDB, map[string]string{"CF-IPCountry": "XX"}, berlinIP, location{"EU", "DE", "Land Berlin", "Berlin"}},
		{"neither knows: headers kept", withDB, map[string]string{"CF-IPContinent": "AS"}, "203.0.113.1", location{"AS", "", "", ""}},
		{"no headers configured: database", dbOnly, all, parisIP, location{"EU", "FR", "Île-de-France", "Paris"}},
		{"nothing configured", &Tracker{}, all, berlinIP, location{}},
		{"invalid header values dropped", headersOnly, map[string]string{"CF-IPCountry": "USA", "CF-IPContinent": "EUR", "CF-Region": "a\x00b", "CF-IPCity": "Z\xfcrich"}, berlinIP, location{}},
	}
	for _, c := range cases {
		if got := c.tr.location(req(c.h), c.ip); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestCleanPlaceAndContinent(t *testing.T) {
	for in, want := range map[string]string{" São Paulo ": "São Paulo", "": "", "a\tb": "", "\xff": "", strings.Repeat("a", 120): strings.Repeat("a", maxPlace)} {
		if got := cleanPlace(in); got != want {
			t.Errorf("place %q -> %q", in, got)
		}
	}
	for in, want := range map[string]string{"af": "AF", "AN": "AN", " as ": "AS", "EU": "EU", "NA": "NA", "OC": "OC", "SA": "SA", "T1": "", "XX": "", "": ""} {
		if got := cleanContinent(in); got != want {
			t.Errorf("continent %q -> %q", in, got)
		}
	}
}

// End to end with both sources configured: the location reaches the row, and
// still the raw IP reaches neither a row nor OnError.
func TestLocationIsStoredAndTheIPIsNot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "city.mmdb")
	writeFile(t, path, fixtureDB(t), time.Unix(1, 0))
	db := newFakeDB()
	log := &errLog{}
	tr, _ := newTracker(t, db, Config{
		GeoDB: path, OnError: log.add,
		CountryHeader: CloudflareCountryHeader, ContinentHeader: CloudflareContinentHeader,
		RegionHeader: CloudflareRegionHeader, CityHeader: CloudflareCityHeader,
		ClientIP: func(*http.Request) string { return berlinIP },
	})
	post(tr, `{"t":"pageview","p":"/"}`, chromeUA)
	r := httptest.NewRequest(http.MethodPost, "/_s", strings.NewReader(`{"t":"pageview","p":"/b"}`))
	r.Host = "example.com"
	r.Header.Set("User-Agent", chromeUA)
	r.Header.Set("CF-IPCountry", "JP")
	r.Header.Set("CF-IPContinent", "AS")
	r.Header.Set("CF-Region", "Tokyo")
	r.Header.Set("CF-IPCity", "Tokyo")
	tr.ServeHTTP(httptest.NewRecorder(), r)

	rows := flushed(t, tr, db)
	if len(rows) != 1 || len(rows[0]) != 2*len(eventColumns) {
		t.Fatalf("rows %d", len(rows))
	}
	col := func(row int, name string) any {
		for i, c := range eventColumns {
			if c == name {
				return rows[0][row*len(eventColumns)+i]
			}
		}
		t.Fatalf("no column %s", name)
		return nil
	}
	for row, want := range [][4]string{{"EU", "DE", "Land Berlin", "Berlin"}, {"AS", "JP", "Tokyo", "Tokyo"}} {
		got := [4]any{col(row, "continent"), col(row, "country"), col(row, "region"), col(row, "city")}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d: %v, want %v", row, got, want)
				break
			}
		}
	}
	for _, c := range ipColumns(rows, berlinIP) {
		t.Errorf("column %s carries the raw IP", c)
	}
	if log.n() != 0 {
		t.Errorf("errors %v", log.text())
	}
}
