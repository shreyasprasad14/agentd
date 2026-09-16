// Package ui serves the single-page trajectory viewer: the run list, one run's
// event timeline, and the SSE tail that keeps it current.
//
// It is three files of HTML, CSS and JavaScript compiled into the binary with
// go:embed. No build step, no framework, no CDN, no npm — a project whose
// whole claim is that it ships as one binary and a Postgres URL should not
// need a node_modules to display its own output (plan M6). The cost is paid in
// this package's own DOM helpers, which is a few dozen lines; the alternative
// buys a bundler, a lockfile and a second language's dependency surface to
// render a list and a log.
//
// The viewer talks only to this API: /v1/runs, /v1/runs/:id, its /events,
// /stream, /cancel and /trace. Nothing it loads comes from the network, which
// is both a demo requirement (it has to work on a laptop with no internet) and
// what makes the Content-Security-Policy below strict enough to be worth
// setting.
package ui

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// assets holds the viewer. The patterns are listed one by one rather than as a
// glob so that adding a file is a deliberate act: an embed directive matching
// "*" would happily ship an editor backup or a scratch file that happened to
// be in the directory at build time.
//
//go:embed index.html app.js styles.css
var assets embed.FS

// contentSecurityPolicy is the viewer's second line of defense against the
// text it renders.
//
// The first line is app.js, which builds DOM and never parses markup; this is
// what is left if that is ever wrong. 'self' is the whole policy because the
// page loads nothing from anywhere else — no CDN, no webfont, no inline script
// or style — so the usual reasons to weaken it with 'unsafe-inline' do not
// arise. img-src allows data: for the one inline SVG favicon.
//
// base-uri and form-action close the two injection routes a CSP built only out
// of script-src leaves open: a <base> tag that repoints every relative URL on
// the page, and a form that posts somewhere else. frame-ancestors keeps a run's
// trajectory from being framed by a page that wants it to look like its own.
//
// Top-level navigation is deliberately unrestricted: the Jaeger deep link
// leaves this origin by design, and the handler for it validates the scheme.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' data:; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// FS returns the embedded assets, rooted at the viewer's directory: index.html,
// app.js and styles.css sit at the top level of it.
//
// It exists for callers that want to serve the files some other way — behind a
// prefix, through their own middleware, or in a test — without reaching for
// the unexported variable.
func FS() fs.FS { return assets }

// Handler serves the viewer. Mount it at the root of the API: index.html is
// returned for "/", and app.js and styles.css for their own paths.
//
// The assets reference each other relatively ("app.js", not "/app.js"), so a
// prefix mount works too as long as the prefix is stripped before this handler
// sees the request.
//
// Nothing here sets a cache header. An embed.FS reports a zero ModTime, so
// http.FileServerFS sends no Last-Modified and every reload refetches — which
// for a tool whose assets change only when the binary is rebuilt is the right
// default: three files over localhost cost nothing, and a stale viewer served
// out of a browser cache after a rebuild is a debugging trap that costs an
// afternoon. An ETag keyed on the build would be the fix if this were ever
// served across a real network.
func Handler() http.Handler {
	files := http.FileServerFS(FS())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", contentSecurityPolicy)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// The trace link navigates to Jaeger, and a run id in a Referer header
		// is a run id in someone else's access log.
		w.Header().Set("Referrer-Policy", "no-referrer")
		files.ServeHTTP(w, r)
	})
}

// Mount registers the viewer as the router's catch-all, so that "/" serves the
// page and every asset path resolves next to it.
//
// The catch-all does not shadow the API: chi matches static path segments
// before a wildcard whatever the registration order, so /v1/runs and /healthz
// keep their handlers and only paths nothing else claims reach the viewer.
// Registration order is therefore not load-bearing — embed_test.go pins that
// behaviour in both orders rather than trusting this comment — but calling it
// last still reads the way routers are meant to be read.
//
// The wildcard also means an unknown path under the API gets the file server's
// 404 rather than chi's. Rejected alternative: serving index.html for anything
// unmatched, SPA-style. It would answer a typo'd GET /v1/runz with 200 and a
// page of HTML, which is a worse answer to a broken client than a 404.
func Mount(r chi.Router) {
	r.Handle("/*", Handler())
}
