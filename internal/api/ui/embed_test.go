package ui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

// assetNames is every file the viewer needs to be a viewer. A missing one is a
// blank page served with a 200, which is the failure mode this test exists to
// turn into a build failure: go:embed does not complain about a file that is
// there but empty, and nothing else in the binary reads these bytes.
var assetNames = []string{"index.html", "app.js", "styles.css"}

func TestEmbeddedAssets(t *testing.T) {
	for _, name := range assetNames {
		body, err := fs.ReadFile(FS(), name)
		require.NoErrorf(t, err, "%s is not embedded", name)
		require.NotEmptyf(t, body, "%s is embedded but empty", name)
	}

	index, err := fs.ReadFile(FS(), "index.html")
	require.NoError(t, err)
	// The shell references the other two by name; a rename that updated only
	// the embed directive would still build and still serve nothing useful.
	require.Contains(t, string(index), `src="app.js"`)
	require.Contains(t, string(index), `href="styles.css"`)
}

func TestHandlerServesViewer(t *testing.T) {
	h := Handler()

	res := get(t, h, "/")
	require.Equal(t, http.StatusOK, res.Code)
	require.Contains(t, res.Header().Get("Content-Type"), "text/html")
	require.Contains(t, res.Body.String(), "trajectory viewer")
	// The policy is half of the XSS story (see contentSecurityPolicy), so a
	// handler that quietly stopped sending it should fail here.
	require.Equal(t, contentSecurityPolicy, res.Header().Get("Content-Security-Policy"))
	require.Equal(t, "nosniff", res.Header().Get("X-Content-Type-Options"))

	res = get(t, h, "/app.js")
	require.Equal(t, http.StatusOK, res.Code)
	require.Contains(t, res.Header().Get("Content-Type"), "javascript")

	res = get(t, h, "/styles.css")
	require.Equal(t, http.StatusOK, res.Code)
	require.Contains(t, res.Header().Get("Content-Type"), "css")

	res = get(t, h, "/does-not-exist.js")
	require.Equal(t, http.StatusNotFound, res.Code)
}

// TestMountDoesNotShadowAPIRoutes pins the claim Mount's doc comment makes:
// the viewer's catch-all loses to any static route, in either registration
// order. If chi's precedence ever changed, mounting the viewer would silently
// swallow the entire API, and the first symptom would be a curl returning HTML.
func TestMountDoesNotShadowAPIRoutes(t *testing.T) {
	api := func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("api")) }

	for _, tc := range []struct {
		name  string
		build func() chi.Router
	}{
		{"viewer last", func() chi.Router {
			r := chi.NewRouter()
			r.Get("/v1/runs", api)
			r.Get("/healthz", api)
			Mount(r)
			return r
		}},
		{"viewer first", func() chi.Router {
			r := chi.NewRouter()
			Mount(r)
			r.Get("/v1/runs", api)
			r.Get("/healthz", api)
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.build()

			for _, path := range []string{"/v1/runs", "/healthz"} {
				res := get(t, r, path)
				require.Equal(t, http.StatusOK, res.Code)
				require.Equal(t, "api", res.Body.String(), "%s must reach the API handler", path)
			}

			res := get(t, r, "/")
			require.Equal(t, http.StatusOK, res.Code)
			require.Contains(t, res.Body.String(), "trajectory viewer")

			res = get(t, r, "/styles.css")
			require.Equal(t, http.StatusOK, res.Code)

			// An unmatched path under the API gets a 404 rather than the page:
			// the viewer is a catch-all, not an SPA rewrite.
			res = get(t, r, "/v1/runz")
			require.Equal(t, http.StatusNotFound, res.Code)
			require.NotContains(t, res.Body.String(), "trajectory viewer")
		})
	}
}

// markupSinks are the DOM APIs that parse a string as HTML or as code.
//
// The patterns match uses, not mentions: ".innerHTML =" rather than
// "innerHTML", so the comments in app.js can name what they are forbidding
// without tripping the test that enforces it.
var markupSinks = []*regexp.Regexp{
	regexp.MustCompile(`\.innerHTML\s*=`),
	regexp.MustCompile(`\.outerHTML\s*=`),
	regexp.MustCompile(`insertAdjacentHTML\s*\(`),
	regexp.MustCompile(`document\.write(?:ln)?\s*\(`),
	regexp.MustCompile(`\beval\s*\(`),
	regexp.MustCompile(`new\s+Function\s*\(`),
	// An inline handler attribute is both a sink and a Content-Security-Policy
	// violation, so it would fail silently in a browser and loudly here.
	regexp.MustCompile(`\son[a-z]+\s*=\s*["']`),
}

// TestViewerUsesNoMarkupSinks is the regression test for the reason the plan
// gives for building this page carefully at all.
//
// Everything the viewer renders — a goal, model output, tool arguments, a tool
// result, an error string — is attacker-influenced by construction; the eval
// corpus contains poisoned documents on purpose. agentd's own UI executing the
// injection its eval suite exists to prove the agent resists would not be a
// style nit, it would be the project refuting its own thesis in the one place
// everybody looks. A grep is a crude enforcement mechanism, but it is one that
// survives a hurried patch six months from now, which a code review does not.
func TestViewerUsesNoMarkupSinks(t *testing.T) {
	for _, name := range []string{"app.js", "index.html"} {
		body, err := fs.ReadFile(FS(), name)
		require.NoError(t, err)
		for _, sink := range markupSinks {
			require.NotRegexpf(t, sink, string(body),
				"%s uses %s: every dynamic string must go in through textContent", name, sink)
		}
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	res := httptest.NewRecorder()
	h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
	return res
}
