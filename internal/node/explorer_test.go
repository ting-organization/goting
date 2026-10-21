package node

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/ting-organization/goting/internal/chain"
	"github.com/ting-organization/goting/internal/types"
)

func TestExplorerRoutes(t *testing.T) {
	c, _ := chain.New(types.DevelopmentGenesis(), chain.NoSeed, chain.NewMemStore(), chain.DefaultConfig())
	n := New(c, Config{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(n.API(APIOptions{}))
	defer srv.Close()
	get := func(path string) (*http.Response, string) {
		t.Helper()
		r, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r, string(b)
	}
	for _, p := range []string{"/", "/blocks", "/blocks/12", "/transactions/" + strings.Repeat("ab", 32),
		"/contracts/" + strings.Repeat("cd", 32), "/mempool", "/network", "/insights"} {
		r, body := get(p)
		if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/html") ||
			!strings.Contains(r.Header.Get("Content-Security-Policy"), "script-src 'self'") || !strings.Contains(body, "Ting explorer") {
			t.Fatalf("%s: %d %s", p, r.StatusCode, r.Header.Get("Content-Type"))
		}
	}
	_, page := get("/")
	m := regexp.MustCompile(`/assets/app\.js\?v=[0-9a-f]{12}`).FindString(page)
	if m == "" {
		t.Fatal("the page does not version its script")
	}
	r, js := get(m)
	if r.StatusCode != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/javascript") ||
		!strings.Contains(r.Header.Get("Cache-Control"), "immutable") || !strings.Contains(js, "verifyProof") {
		t.Fatalf("script: %d %s %s", r.StatusCode, r.Header.Get("Content-Type"), r.Header.Get("Cache-Control"))
	}
	for _, p := range []string{"/assets/index.html", "/assets/nope.js", "/assets/x.exe"} {
		if r, _ := get(p); r.StatusCode != 404 {
			t.Fatalf("%s: %d", p, r.StatusCode)
		}
	}
	if r, body := get("/v1/status"); r.StatusCode != 200 || !strings.Contains(body, `"network"`) {
		t.Fatal("the API moved")
	}
	if r, body := get("/elsewhere"); r.StatusCode != 404 || !strings.Contains(body, "endpoint not found") {
		t.Fatal("fallback")
	}
}
