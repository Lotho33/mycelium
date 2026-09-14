package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeIndexHTML mimics a `flutter build web` output: <base href="/"> plus a
// relative-path script tag, the exact combination that produced the
// production bug (root-relative asset requests instead of {prefix}/asset).
const fakeIndexHTML = `<!DOCTYPE html>
<html>
<head>
  <base href="/">
  <title>Pileus</title>
</head>
<body>
  <script src="hls.min.js"></script>
  <script src="flutter_bootstrap.js" async></script>
</body>
</html>
`

const fakeBootstrapJS = `// fake flutter_bootstrap.js content, byte-for-byte check`

func newWebAppServer(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(fakeIndexHTML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "flutter_bootstrap.js"), []byte(fakeBootstrapJS), 0o644); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	serveWebApp(mux, prefix, dir)
	return httptest.NewServer(mux)
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestServeWebApp_RewritesBaseHref(t *testing.T) {
	for _, prefix := range []string{"/app", "/app-tv"} {
		t.Run(prefix, func(t *testing.T) {
			srv := newWebAppServer(t, prefix)
			defer srv.Close()

			status, body := getBody(t, srv.URL+prefix+"/")
			if status != http.StatusOK {
				t.Fatalf("GET %s/: status = %d, body = %q", prefix, status, body)
			}
			if strings.Contains(body, `<base href="/">`) {
				t.Errorf("GET %s/: base href not rewritten, still root: %q", prefix, body)
			}
			want := `<base href="` + prefix + `/">`
			if !strings.Contains(body, want) {
				t.Errorf("GET %s/: body missing %q\nbody: %s", prefix, want, body)
			}
			// The rest of index.html (asset tags) must survive untouched.
			if !strings.Contains(body, `src="hls.min.js"`) || !strings.Contains(body, `src="flutter_bootstrap.js"`) {
				t.Errorf("GET %s/: asset tags altered unexpectedly: %s", prefix, body)
			}
		})
	}
}

func TestServeWebApp_SPAFallbackRewritesBaseHref(t *testing.T) {
	for _, prefix := range []string{"/app", "/app-tv"} {
		t.Run(prefix, func(t *testing.T) {
			srv := newWebAppServer(t, prefix)
			defer srv.Close()

			// A client-side Flutter route that doesn't exist on disk must
			// still fall back to index.html, rewritten the same way.
			status, body := getBody(t, srv.URL+prefix+"/qualunque-rotta-flutter")
			if status != http.StatusOK {
				t.Fatalf("GET %s/qualunque-rotta-flutter: status = %d, body = %q", prefix, status, body)
			}
			want := `<base href="` + prefix + `/">`
			if !strings.Contains(body, want) {
				t.Errorf("SPA fallback: body missing %q\nbody: %s", want, body)
			}
		})
	}
}

func TestServeWebApp_StaticAssetUntouched(t *testing.T) {
	for _, prefix := range []string{"/app", "/app-tv"} {
		t.Run(prefix, func(t *testing.T) {
			srv := newWebAppServer(t, prefix)
			defer srv.Close()

			status, body := getBody(t, srv.URL+prefix+"/flutter_bootstrap.js")
			if status != http.StatusOK {
				t.Fatalf("GET %s/flutter_bootstrap.js: status = %d, body = %q", prefix, status, body)
			}
			if body != fakeBootstrapJS {
				t.Errorf("static asset was altered:\n got: %q\nwant: %q", body, fakeBootstrapJS)
			}
		})
	}
}
