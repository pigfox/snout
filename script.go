package snout

import _ "embed"

// Script is the browser snippet. Serve it from your own origin and load it
// with the collector path in data-endpoint:
//
//	<script defer src="/js/snout.js" data-endpoint="/_s"></script>
//
// It sends a pageview with path, referrer host, UTM parameters, screen width
// and title; an engagement beacon carrying visible time when the page is
// hidden; and whatever window.snout.event(name, props) is called with. It
// waits out prerendering, sets no cookie and touches no storage.
//
//go:embed snout.js
var Script []byte
