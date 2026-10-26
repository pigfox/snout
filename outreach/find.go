package main

import (
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// contactPaths are the pages checked, in order, for a published address.
var contactPaths = []string{"", "/contact", "/contact-us", "/contact/", "/about", "/privacy-policy", "/privacy"}

var emailRE = regexp.MustCompile(`(?i)[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}`)

// preferred local parts, best first; anything else on the site's domain ranks after.
var preferred = []string{"hello", "info", "contact", "sales", "partnerships", "marketing", "team", "inquiries", "growth", "new"}

// findEmails fills the empty email cells of the list with an address published
// on the agency's own site and on its own domain, and records the page it came
// from in email_source. Rows that already have an email are left alone.
func findEmails(path string, client *http.Client, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	rows, err := csv.NewReader(f).ReadAll()
	f.Close()
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	col := map[string]int{}
	for i, h := range rows[0] {
		col[strings.ToLower(strings.TrimSpace(h))] = i
	}
	for _, k := range []string{"website", "email", "email_source"} {
		if _, ok := col[k]; !ok {
			col[k] = len(rows[0])
			rows[0] = append(rows[0], k)
		}
	}
	width := len(rows[0])
	found := 0
	for i := 1; i < len(rows); i++ {
		for len(rows[i]) < width {
			rows[i] = append(rows[i], "")
		}
		r := rows[i]
		if strings.TrimSpace(r[col["email"]]) != "" || strings.TrimSpace(r[col["website"]]) == "" {
			continue
		}
		email, src := scrape(client, r[col["website"]])
		if email == "" {
			fmt.Fprintf(out, "none  %s\n", r[col["website"]])
			continue
		}
		r[col["email"]], r[col["email_source"]] = email, src
		found++
		fmt.Fprintf(out, "found %s  (%s)\n", email, src)
	}
	fmt.Fprintf(out, "%d new addresses\n", found)

	tmp := path + ".tmp"
	w, err := os.Create(tmp)
	if err != nil {
		return err
	}
	cw := csv.NewWriter(w)
	cw.WriteAll(rows)
	if err := cw.Error(); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// scrape returns the best address on the site's own domain and the page it
// was published on, or "" if none of the contact pages show one.
func scrape(client *http.Client, website string) (string, string) {
	if !strings.Contains(website, "://") {
		website = "https://" + website
	}
	base, err := url.Parse(website)
	if err != nil {
		return "", ""
	}
	site := strings.TrimPrefix(strings.ToLower(base.Hostname()), "www.")
	var best, bestSrc string
	bestRank := len(preferred) + 1
	for _, p := range contactPaths {
		page := base.Scheme + "://" + base.Host + p
		body, final := get(client, page)
		if final != "" {
			// a redirect to a new domain (rebrand) makes that the site's domain
			if h := strings.TrimPrefix(strings.ToLower(final), "www."); h != site && p == "" {
				site = h
			}
		}
		for _, e := range candidates(body, site) {
			if r := rank(e); r < bestRank {
				best, bestSrc, bestRank = e, page, r
			}
		}
		if bestRank == 0 {
			break
		}
	}
	return best, bestSrc
}

func get(client *http.Client, page string) (string, string) {
	req, err := http.NewRequest("GET", page, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; trireens-outreach/1.0)")
	resp, err := client.Do(req)
	if err != nil {
		return "", ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	return string(b), resp.Request.URL.Hostname()
}

// candidates returns the distinct addresses in a page on site or a subdomain
// of it, decoding the &#64; and %40 forms of @.
func candidates(body, site string) []string {
	body = strings.NewReplacer("&#64;", "@", "&#064;", "@", "%40", "@", "&commat;", "@").Replace(body)
	seen := map[string]bool{}
	var out []string
	for _, e := range emailRE.FindAllString(body, -1) {
		e = strings.ToLower(strings.TrimRight(e, "."))
		_, d, _ := strings.Cut(e, "@")
		if d != site && !strings.HasSuffix(d, "."+site) {
			continue
		}
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	sort.Strings(out)
	return out
}

func rank(email string) int {
	local, _, _ := strings.Cut(email, "@")
	for i, p := range preferred {
		if local == p {
			return i
		}
	}
	for _, skip := range []string{"privacy", "legal", "dmca", "abuse", "careers", "jobs", "hr", "billing", "accounting", "noreply", "no-reply", "support"} {
		if strings.Contains(local, skip) {
			return len(preferred) + 1 // never chosen
		}
	}
	return len(preferred)
}

func newClient() *http.Client { return &http.Client{Timeout: 15 * time.Second} }
