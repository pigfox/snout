/*! snout v0.1.2 | MIT | https://github.com/pigfox/snout
 * Cookieless first-party analytics. No cookies, no storage, no identifiers.
 * Load with: <script defer src="/js/snout.js" data-endpoint="/_s"></script>
 */
(function () {
  "use strict";
  var d = document, n = navigator, s = d.currentScript;
  var ep = s && s.getAttribute("data-endpoint");
  if (!ep || !n.sendBeacon) return;

  var UTM = ["source", "medium", "campaign", "term", "content"];

  // An automated browser (headless Chrome, Lighthouse, Playwright, Puppeteer)
  // can send a plain browser User-Agent, so the User-Agent filter cannot tell
  // it apart. It sets navigator.webdriver; and an emulated brand list, such as
  // Lighthouse's phone or Mac, lacks the "Not.A.Brand" entry every Chromium
  // build adds. Either way nothing is sent. window.snout is still defined so a
  // page calling snout.event() does not throw.
  var brands = n.userAgentData && n.userAgentData.brands;
  var robot = n.webdriver === true || !!(brands && brands.length &&
    !brands.some(function (b) { return /Not.A.Brand/.test(b.brand); }));

  function send(o) {
    if (robot) return;
    try { n.sendBeacon(ep, JSON.stringify(o)); } catch (e) { /* never break the page */ }
  }

  // Only the UTM keys leave the page; every other query parameter is ignored.
  function utm() {
    var q = new URLSearchParams(location.search), o = {}, any = false;
    for (var i = 0; i < UTM.length; i++) {
      var v = q.get("utm_" + UTM[i]);
      if (v) { o[UTM[i]] = v.slice(0, 200); any = true; }
    }
    return any ? o : undefined;
  }

  // The referrer is reduced to its host before it is sent.
  function ref() {
    try { return d.referrer ? new URL(d.referrer).host : undefined; } catch (e) { return undefined; }
  }

  var since = null, engaged = 0;
  function start() { if (d.visibilityState === "visible" && since === null) since = Date.now(); }
  function stop() { if (since !== null) { engaged += Date.now() - since; since = null; } }

  function pageview() {
    send({ t: "pageview", p: location.pathname, r: ref(), u: utm(), w: screen.width, ti: d.title });
    start();
  }

  d.addEventListener("visibilitychange", function () {
    if (d.visibilityState === "hidden") {
      stop();
      if (engaged > 0) { send({ t: "engagement", p: location.pathname, e: engaged }); engaged = 0; }
    } else {
      start();
    }
  });

  window.snout = {
    event: function (name, props) {
      if (typeof name === "string" && name) send({ t: "event", p: location.pathname, n: name, pr: props });
    }
  };

  // A prerendered page has not been seen; count it when it is activated.
  if (d.prerendering) {
    d.addEventListener("prerenderingchange", pageview, { once: true });
  } else {
    pageview();
  }
})();
