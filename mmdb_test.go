package snout

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"sort"
	"testing"

	"github.com/oschwald/maxminddb-golang"
)

// buildMMDB writes a minimal MaxMind DB (format 2.0, record size 32) mapping
// each prefix to its record, so the tests need no binary fixture and no
// writer dependency. ipVersion 6 places IPv4 under ::/96, as real City
// databases do; ipVersion 4 accepts IPv4 prefixes only.
func buildMMDB(t *testing.T, ipVersion int, recs map[string]map[string]any) []byte {
	t.Helper()
	type rec struct{ next, data int } // next: node index or -1; data: index or -1
	nodes := [][2]rec{{{-1, -1}, {-1, -1}}}
	var data [][]byte

	prefixes := make([]string, 0, len(recs))
	for p := range recs {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	for _, ps := range prefixes {
		p := netip.MustParsePrefix(ps)
		var bits []byte
		plen := p.Bits()
		if ipVersion == 6 {
			a := p.Addr().As16()
			if p.Addr().Is4() {
				a = [16]byte{}
				v4 := p.Addr().As4()
				copy(a[12:], v4[:])
				plen += 96
			}
			bits = a[:]
		} else {
			a := p.Addr().As4()
			bits = a[:]
		}
		data = append(data, mmdbEncode(recs[ps]))
		n := 0
		for i := 0; i < plen; i++ {
			b := bits[i/8] >> (7 - i%8) & 1
			if i == plen-1 {
				nodes[n][b] = rec{-1, len(data) - 1}
				break
			}
			if nodes[n][b].next < 0 {
				nodes = append(nodes, [2]rec{{-1, -1}, {-1, -1}})
				nodes[n][b] = rec{len(nodes) - 1, -1}
			}
			n = nodes[n][b].next
		}
	}

	offsets := make([]int, len(data))
	var section []byte
	for i, d := range data {
		offsets[i] = len(section)
		section = append(section, d...)
	}
	count := len(nodes)
	var out bytes.Buffer
	for _, nd := range nodes {
		for _, r := range nd {
			v := count // empty
			switch {
			case r.next >= 0:
				v = r.next
			case r.data >= 0:
				v = count + 16 + offsets[r.data]
			}
			_ = binary.Write(&out, binary.BigEndian, uint32(v))
		}
	}
	out.Write(make([]byte, 16))
	out.Write(section)
	out.WriteString("\xAB\xCD\xEFMaxMind.com")
	out.Write(mmdbEncode(map[string]any{
		"binary_format_major_version": uint16(2),
		"binary_format_minor_version": uint16(0),
		"build_epoch":                 uint32(1762387200),
		"database_type":               "Test-City",
		"description":                 map[string]any{"en": "snout test"},
		"ip_version":                  uint16(ipVersion),
		"languages":                   []any{"en"},
		"node_count":                  uint32(count),
		"record_size":                 uint16(32),
	}))
	return out.Bytes()
}

// mmdbEncode encodes the data-section types the fixtures use.
func mmdbEncode(v any) []byte {
	ctrl := func(typ, size int) []byte {
		var h []byte
		lead := byte(typ << 5)
		ext := typ > 7
		if ext {
			lead = 0
		}
		switch {
		case size < 29:
			h = []byte{lead | byte(size)}
		case size < 285:
			h = []byte{lead | 29, byte(size - 29)}
		default:
			s := size - 285
			h = []byte{lead | 30, byte(s >> 8), byte(s)}
		}
		if ext {
			h = append(h[:1], append([]byte{byte(typ - 7)}, h[1:]...)...)
		}
		return h
	}
	uint := func(typ int, x uint64, width int) []byte {
		b := make([]byte, 8)
		binary.BigEndian.PutUint64(b, x)
		b = b[8-width:]
		for len(b) > 0 && b[0] == 0 {
			b = b[1:]
		}
		return append(ctrl(typ, len(b)), b...)
	}
	switch x := v.(type) {
	case string:
		return append(ctrl(2, len(x)), x...)
	case uint16:
		return uint(5, uint64(x), 2)
	case uint32:
		return uint(6, uint64(x), 4)
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := ctrl(7, len(x))
		for _, k := range keys {
			out = append(out, mmdbEncode(k)...)
			out = append(out, mmdbEncode(x[k])...)
		}
		return out
	case []any:
		out := ctrl(11, len(x))
		for _, e := range x {
			out = append(out, mmdbEncode(e)...)
		}
		return out
	}
	panic("mmdbEncode: unsupported type")
}

// cityRecord is shaped like a DB-IP City Lite / GeoLite2 City record,
// including the location field snout must never read.
func cityRecord(continent, country, region, city string) map[string]any {
	r := map[string]any{
		"continent": map[string]any{"code": continent, "names": map[string]any{"en": "x"}},
		"country":   map[string]any{"iso_code": country, "names": map[string]any{"en": "x"}},
		"location":  map[string]any{"time_zone": "Europe/Berlin"},
	}
	if region != "" {
		r["subdivisions"] = []any{map[string]any{"names": map[string]any{"en": region}}}
	}
	if city != "" {
		r["city"] = map[string]any{"names": map[string]any{"en": city}}
	}
	return r
}

// The builder is only useful if the real reader agrees with it.
func TestBuildMMDBIsReadable(t *testing.T) {
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	b := buildMMDB(t, 6, map[string]map[string]any{
		"198.51.100.0/24": cityRecord("EU", "DE", "Land Berlin", "Berlin"),
		"2001:db8::/32":   cityRecord("NA", "US", "Texas", string(long)),
		"192.0.2.0/24":    {"n": uint32(70000), "s": string(long[:40])},
	})
	r, err := maxminddb.FromBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	var rec geoRecord
	if err := r.Lookup(netip.MustParseAddr("198.51.100.9").AsSlice(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Country.ISOCode != "DE" || rec.City.Names["en"] != "Berlin" || rec.Subdivisions[0].Names["en"] != "Land Berlin" {
		t.Errorf("%+v", rec)
	}
	var v6 geoRecord
	if err := r.Lookup(netip.MustParseAddr("2001:db8::1").AsSlice(), &v6); err != nil || len(v6.City.Names["en"]) != 300 {
		t.Errorf("v6 %v %d", err, len(v6.City.Names["en"]))
	}
	var other struct {
		N uint32 `maxminddb:"n"`
	}
	if err := r.Lookup(netip.MustParseAddr("192.0.2.1").AsSlice(), &other); err != nil || other.N != 70000 {
		t.Errorf("uint32 %v %d", err, other.N)
	}
	if r.Metadata.DatabaseType != "Test-City" || r.Metadata.IPVersion != 6 {
		t.Errorf("metadata %+v", r.Metadata)
	}
}
