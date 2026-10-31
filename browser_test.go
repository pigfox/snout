package snout

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests drive real browsers, so they run only with
// SNOUT_BROWSER_TESTS=1: Lighthouse needs npx and Chrome, and ordinary
// Chrome needs a display.
//
//	SNOUT_BROWSER_TESTS=1 go test -run Browser -v -timeout 10m .
func browserTests(t *testing.T) {
	t.Helper()
	if os.Getenv("SNOUT_BROWSER_TESTS") != "1" {
		t.Skip("set SNOUT_BROWSER_TESTS=1 to drive real browsers")
	}
}

// site serves a page that loads script and posts to a fresh Tracker, and
// counts the beacons that reach the collector.
type site struct {
	url   string
	tr    *Tracker
	db    *fakeDB
	posts atomic.Int64
}

func newSite(t *testing.T, script []byte) *site {
	t.Helper()
	s := &site{db: newFakeDB()}
	s.tr, _ = newTracker(t, s.db, Config{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width"><title>snout browser test page</title>
<script defer src="/js/snout.js" data-endpoint="/_s"></script></head>
<body><h1>snout</h1><p>A page for the browser tests.</p></body></html>`))
	})
	mux.HandleFunc("/js/snout.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript")
		_, _ = w.Write(script)
	})
	mux.HandleFunc("/_s", func(w http.ResponseWriter, r *http.Request) {
		s.posts.Add(1)
		s.tr.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	s.url = srv.URL + "/"
	return s
}

// lighthouse runs Lighthouse exactly as the estate policy pins it.
func lighthouse(t *testing.T, url string, desktop bool) {
	t.Helper()
	args := []string{"-y", "lighthouse@13.5.0", url}
	if desktop {
		args = append(args, "--preset=desktop")
	}
	args = append(args, "--quiet", "--chrome-flags=--headless=new", "--output=json",
		"--output-path="+filepath.Join(t.TempDir(), "report.json"))
	if out, err := exec.Command("npx", args...).CombinedOutput(); err != nil {
		t.Fatalf("lighthouse: %v\n%s", err, out)
	}
}

// LIGHTHOUSE IS NOT COUNTED: a mobile and a desktop run of Lighthouse 13.5.0
// store nothing. The real script sends no beacon at all; with the script's
// check removed, the collector still drops every beacon as a bot.
func TestBrowserLighthouseNotCounted(t *testing.T) {
	browserTests(t)

	s := newSite(t, Script)
	lighthouse(t, s.url, false)
	lighthouse(t, s.url, true)
	if n := s.posts.Load(); n != 0 {
		t.Errorf("script: %d beacons sent, want 0", n)
	}
	if rows := flushed(t, s.tr, s.db); len(rows) != 0 {
		t.Errorf("script: %d rows stored", len(rows))
	}

	unguarded := []byte(strings.Replace(string(Script), "if (robot) return;", "", 1))
	if string(unguarded) == string(Script) {
		t.Fatal("the script's robot check moved; update this test")
	}
	s = newSite(t, unguarded)
	lighthouse(t, s.url, false)
	lighthouse(t, s.url, true)
	if s.posts.Load() == 0 {
		t.Fatal("collector: no beacon reached it, so it was not exercised")
	}
	if st := s.tr.Stats(); st.Accepted != 0 || st.Bots != uint64(s.posts.Load()) {
		t.Errorf("collector: %d beacons, %d accepted, %d bots", s.posts.Load(), st.Accepted, st.Bots)
	}
	if rows := flushed(t, s.tr, s.db); len(rows) != 0 {
		t.Errorf("collector: %d rows stored", len(rows))
	}
}

// ORDINARY CHROME IS COUNTED: Chrome started the way a person starts it,
// with a window, a fresh profile and no automation flags, records a pageview.
func TestBrowserOrdinaryChromeCounted(t *testing.T) {
	browserTests(t)
	chrome, err := exec.LookPath("google-chrome")
	if err != nil {
		t.Skip("google-chrome not installed")
	}
	if os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		t.Skip("no display for a windowed Chrome")
	}

	s := newSite(t, Script)
	cmd := exec.Command(chrome, "--user-data-dir="+t.TempDir(), "--no-first-run",
		"--no-default-browser-check", "--new-window", s.url)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for s.tr.Stats().Accepted == 0 && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if st := s.tr.Stats(); st.Accepted == 0 || st.Bots != 0 {
		t.Fatalf("accepted %d, bots %d: ordinary Chrome must be counted", st.Accepted, st.Bots)
	}
	if rows := flushed(t, s.tr, s.db); len(rows) == 0 {
		t.Error("no row stored for ordinary Chrome")
	}
}
