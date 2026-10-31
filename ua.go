package snout

import (
	"regexp"
	"strings"
)

// botMarkers are lowercase User-Agent substrings that identify automated
// clients: crawlers, link previewers, monitors, auditors and HTTP libraries.
// The list is coarse on purpose — a false positive costs one human pageview,
// a false negative puts a robot in the visitor count.
var botMarkers = []string{
	"bot", "crawl", "spider", "slurp", "facebookexternalhit", "preview",
	"lighthouse", "pagespeed", "headlesschrome", "phantomjs", "puppeteer",
	"playwright", "selenium", "webdriver", "curl/", "wget/", "python-",
	"go-http-client", "java/", "okhttp", "axios/", "node-fetch", "uptime",
	"pingdom", "monitor", "render/", "scan",
}

// IsBot reports whether userAgent belongs to an automated client. An empty
// User-Agent is treated as a bot: every real browser sends one.
func IsBot(userAgent string) bool {
	ua := strings.ToLower(strings.TrimSpace(userAgent))
	if ua == "" {
		return true
	}
	for _, m := range botMarkers {
		if strings.Contains(ua, m) {
			return true
		}
	}
	return false
}

// greaseBrand matches the made-up brand every Chromium build adds to its
// brand list ("Not(A:Brand", "Not_A Brand", " Not A;Brand") so servers do
// not hard-code the list. Its punctuation varies by version.
var greaseBrand = regexp.MustCompile(`Not.A.Brand`)

// emulatedBrands reports whether a Sec-CH-UA header lists brands but no
// GREASE brand. Real Chrome, Edge, Brave, Opera and Samsung Internet always
// send one; a tool that rewrites the brand list, as Lighthouse does when it
// emulates a phone or a Mac with a plain Chrome User-Agent, leaves it out.
// An absent header (Firefox, Safari, an insecure origin) says nothing.
func emulatedBrands(secCHUA string) bool {
	s := strings.TrimSpace(secCHUA)
	return s != "" && !greaseBrand.MatchString(s)
}

// deviceClass is GA's device category: mobile, tablet or desktop.
func deviceClass(userAgent string) string {
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "ipad"), strings.Contains(ua, "tablet"),
		strings.Contains(ua, "android") && !strings.Contains(ua, "mobile"):
		return "tablet"
	case strings.Contains(ua, "mobi"), strings.Contains(ua, "iphone"):
		return "mobile"
	default:
		return "desktop"
	}
}

// browserName checks the most specific token first: Edge and Opera also say
// Chrome, and Chrome also says Safari.
func browserName(userAgent string) string {
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "edg/"), strings.Contains(ua, "edga/"), strings.Contains(ua, "edgios/"):
		return "Edge"
	case strings.Contains(ua, "opr/"), strings.Contains(ua, "opera"):
		return "Opera"
	case strings.Contains(ua, "samsungbrowser"):
		return "Samsung Internet"
	case strings.Contains(ua, "firefox/"), strings.Contains(ua, "fxios/"):
		return "Firefox"
	case strings.Contains(ua, "chrome/"), strings.Contains(ua, "crios/"):
		return "Chrome"
	case strings.Contains(ua, "safari/"):
		return "Safari"
	default:
		return "Other"
	}
}

// osName checks iOS before macOS (iPadOS can claim "Mac OS X") and Android
// before Linux (Android says Linux).
func osName(userAgent string) string {
	ua := strings.ToLower(userAgent)
	switch {
	case strings.Contains(ua, "windows"):
		return "Windows"
	case strings.Contains(ua, "iphone"), strings.Contains(ua, "ipad"), strings.Contains(ua, "ipod"):
		return "iOS"
	case strings.Contains(ua, "android"):
		return "Android"
	case strings.Contains(ua, "cros"):
		return "ChromeOS"
	case strings.Contains(ua, "mac os x"), strings.Contains(ua, "macintosh"):
		return "macOS"
	case strings.Contains(ua, "linux"):
		return "Linux"
	default:
		return "Other"
	}
}

// screenBucket coarsens a screen width so the stored value cannot help
// fingerprint a visitor. Zero or negative means the script sent none.
func screenBucket(width int) string {
	switch {
	case width <= 0:
		return ""
	case width < 576:
		return "<576"
	case width < 768:
		return "576-767"
	case width < 1024:
		return "768-1023"
	case width < 1440:
		return "1024-1439"
	default:
		return "1440+"
	}
}
