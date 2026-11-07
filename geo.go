package snout

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/oschwald/maxminddb-golang"
)

// Cloudflare's names for its visitor location headers. CF-IPCountry is sent
// on every plan; the other three only once the "Add visitor location headers"
// managed transform is on. Trust them only when the origin accepts traffic
// from Cloudflare alone.
const (
	CloudflareCountryHeader   = "CF-IPCountry"
	CloudflareContinentHeader = "CF-IPContinent"
	CloudflareRegionHeader    = "CF-Region"
	CloudflareCityHeader      = "CF-IPCity"
)

// DefaultGeoDBCheck is how often the GeoDB file is checked for a change.
const DefaultGeoDBCheck = time.Minute

// maxPlace caps a region or city name, in bytes.
const maxPlace = 100

// continents are the seven codes MaxMind-format databases and Cloudflare use.
var continents = map[string]bool{"AF": true, "AN": true, "AS": true, "EU": true, "NA": true, "OC": true, "SA": true}

// location is where a visit came from, no finer than a city. Latitude,
// longitude and postal code are never read, so they can never be stored.
type location struct {
	continent, country, region, city string
}

// location resolves a request's location: the trusted headers first, then the
// database. The database answers when the headers carry no country, and fills
// what the headers leave empty only when it agrees on the country, so a row
// never mixes two countries' places.
func (t *Tracker) location(r *http.Request, ip string) location {
	l := location{
		continent: cleanContinent(header(r, t.continentHeader)),
		country:   cleanCountry(header(r, t.countryHeader)),
		region:    cleanPlace(header(r, t.regionHeader)),
		city:      cleanPlace(header(r, t.cityHeader)),
	}
	if t.geo == nil || (l.country != "" && l.continent != "" && l.region != "" && l.city != "") {
		return l
	}
	db := t.geo.lookup(ip)
	if l.country == "" {
		if db.country != "" {
			return db
		}
		return l
	}
	if db.country != l.country {
		return l
	}
	if l.continent == "" {
		l.continent = db.continent
	}
	if l.region == "" {
		l.region = db.region
	}
	if l.city == "" {
		l.city = db.city
	}
	return l
}

func header(r *http.Request, name string) string {
	if name == "" {
		return ""
	}
	return r.Header.Get(name)
}

// cleanCountry accepts an ISO 3166-1 alpha-2 code in any case. XX is
// Cloudflare's "unknown" and is treated as no answer.
func cleanCountry(s string) string {
	c := strings.ToUpper(strings.TrimSpace(s))
	if len(c) != countryLen || c[0] < 'A' || c[0] > 'Z' || c[1] < 'A' || c[1] > 'Z' || c == "XX" {
		return ""
	}
	return c
}

func cleanContinent(s string) string {
	c := strings.ToUpper(strings.TrimSpace(s))
	if !continents[c] {
		return ""
	}
	return c
}

// cleanPlace accepts a region or city name: valid UTF-8, no control
// characters, at most maxPlace bytes.
func cleanPlace(s string) string {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return ""
	}
	return truncate(s, maxPlace)
}

// geoRecord is the part of a MaxMind-format City record snout reads. The
// location (latitude, longitude) and postal fields are deliberately absent.
type geoRecord struct {
	Continent struct {
		Code string `maxminddb:"code"`
	} `maxminddb:"continent"`
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"country"`
	Subdivisions []struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"subdivisions"`
	City struct {
		Names map[string]string `maxminddb:"names"`
	} `maxminddb:"city"`
}

// stamp identifies one version of the database file.
type stamp struct {
	missing bool
	mod     time.Time
	size    int64
}

// geoDB is a MaxMind-format database held in memory and reloaded when its file
// changes. The file is read whole rather than memory-mapped, so a file
// rewritten in place can never fault a reader; a reload swaps the reader
// atomically. Any failure to read it turns location off (nil reader) until the
// file changes again.
type geoDB struct {
	path    string
	onError func(error)
	reader  atomic.Pointer[maxminddb.Reader]
	stamp   stamp // owned by reload: New, then the watch goroutine

	done    chan struct{}
	stopped chan struct{}
	once    sync.Once
}

func openGeo(path string, every time.Duration, onError func(error)) *geoDB {
	g := &geoDB{path: path, onError: onError, done: make(chan struct{}), stopped: make(chan struct{})}
	g.reload()
	go g.watch(every)
	return g
}

func (g *geoDB) watch(every time.Duration) {
	defer close(g.stopped)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			g.reload()
		case <-g.done:
			return
		}
	}
}

func (g *geoDB) close() {
	g.once.Do(func() { close(g.done) })
	<-g.stopped
}

// reload reads the file again if it changed since the last attempt. Errors
// name the file, never an IP.
func (g *geoDB) reload() {
	var st stamp
	fi, err := os.Stat(g.path)
	if err != nil {
		st.missing = true
	} else {
		st.mod, st.size = fi.ModTime(), fi.Size()
	}
	if st == g.stamp {
		return
	}
	g.stamp = st
	if err == nil {
		var b []byte
		if b, err = os.ReadFile(g.path); err == nil {
			var r *maxminddb.Reader
			if r, err = maxminddb.FromBytes(b); err == nil {
				g.reader.Store(r)
				return
			}
		}
	}
	g.reader.Store(nil)
	g.onError(fmt.Errorf("snout: geo database off: %w", err))
}

// lookup returns the database's answer for ip, or nothing. A lookup error is
// dropped, never reported: its text can carry the IP.
func (g *geoDB) lookup(ip string) location {
	r := g.reader.Load()
	addr := net.ParseIP(ip)
	if r == nil || addr == nil {
		return location{}
	}
	var rec geoRecord
	if r.Lookup(addr, &rec) != nil {
		return location{}
	}
	l := location{
		continent: cleanContinent(rec.Continent.Code),
		country:   cleanCountry(rec.Country.ISOCode),
		city:      cleanPlace(rec.City.Names["en"]),
	}
	if len(rec.Subdivisions) > 0 {
		l.region = cleanPlace(rec.Subdivisions[0].Names["en"])
	}
	return l
}
