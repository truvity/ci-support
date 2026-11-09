package main

import (
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const ghAsset = "/octo/tool/releases/download/v1.2.3/tool-linux-amd64.tar.gz"

func newGitHubFixture(t *testing.T, up *upstream, extra ...string) *fixture {
	t.Helper()
	return newFixture(t, up, append([]string{
		"-nix=false", "-goproxy=false", "-maven=false", "-github-releases",
		"-github-releases-upstream", up.URL, "-retry-backoff", "1ms",
	}, extra...)...)
}

func TestGitHubMissStoresThenRedirects(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != ghAsset {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("ASSET BYTES"))
	})
	f := newGitHubFixture(t, up)

	r := f.get(t, "/github"+ghAsset)
	wantLoc := "https://store.example/github/octo/tool/releases/download/v1.2.3/tool-linux-amd64.tar.gz?"
	if r.code != http.StatusFound || !strings.HasPrefix(r.header.Get("Location"), wantLoc) {
		t.Fatalf("miss: %d %q, want a redirect to %s...", r.code, r.header.Get("Location"), wantLoc)
	}
	if b, ok := f.store.object("github/octo/tool/releases/download/v1.2.3/tool-linux-amd64.tar.gz"); !ok || string(b) != "ASSET BYTES" {
		t.Fatalf("stored %q, %v; keys %v", b, ok, f.store.keys())
	}
	if up.count(ghAsset) != 1 || up.paths[0] != ghAsset || !strings.HasPrefix(r.header.Get("Cache-Control"), "no-store") {
		t.Errorf("upstream %v, cache-control %q", up.paths, r.header.Get("Cache-Control"))
	}
	m := f.metrics(t)
	for _, want := range []string{
		`cache_redirect_requests_total{frontend="github",result="miss"} 1`,
		`cache_redirect_requests_total{frontend="github",result="redirect"} 0`,
		`cache_redirect_uploads_total{frontend="github",result="ok"} 1`,
		`cache_redirect_upload_bytes_total{frontend="github"} 11`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("/metrics lacks %q", want)
		}
	}
}

func TestGitHubHitIsARedirectWithoutUpstream(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ASSET")) })
	f := newGitHubFixture(t, up)
	if r := f.get(t, "/github"+ghAsset); r.code != http.StatusFound {
		t.Fatalf("miss: %d", r.code)
	}
	before := up.total()
	for i := 0; i < 3; i++ {
		r := f.get(t, "/github"+ghAsset)
		if r.code != http.StatusFound || !strings.HasPrefix(r.header.Get("Location"), "https://store.example/github/octo/tool/") {
			t.Fatalf("hit: %d %q", r.code, r.header.Get("Location"))
		}
	}
	// Owner and repository are case-insensitive at GitHub: the same object.
	if r := f.get(t, "/github/Octo/Tool/releases/download/v1.2.3/tool-linux-amd64.tar.gz"); r.code != http.StatusFound {
		t.Errorf("other case: %d", r.code)
	}
	// A HEAD of a stored asset is answered here, with its size.
	if r := f.do(t, http.MethodHead, "/github"+ghAsset); r.code != 200 || r.header.Get("Content-Length") != "5" {
		t.Errorf("HEAD: %d, Content-Length %q", r.code, r.header.Get("Content-Length"))
	}
	if up.total() != before {
		t.Errorf("a hit went upstream: %v", up.paths)
	}
	if !strings.Contains(f.metrics(t), `cache_redirect_requests_total{frontend="github",result="redirect"} 4`) {
		t.Errorf("hits not counted as redirects:\n%s", f.metrics(t))
	}
}

func TestGitHubRetriesUpstreamFailuresThenSucceeds(t *testing.T) {
	var n counter
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		switch n.inc(); n.get() {
		case 1:
			http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
		case 2:
			http.Error(w, "internal", http.StatusInternalServerError)
		case 3:
			w.Header().Set("Retry-After", "0")
			http.Error(w, "slow down", http.StatusTooManyRequests)
		default:
			_, _ = w.Write([]byte("ASSET"))
		}
	})
	f := newGitHubFixture(t, up)
	r := f.get(t, "/github"+ghAsset)
	if r.code != http.StatusFound || n.get() != 4 {
		t.Fatalf("%d after %d upstream requests, want a redirect after 4", r.code, n.get())
	}
	if _, ok := f.store.object("github/octo/tool/releases/download/v1.2.3/tool-linux-amd64.tar.gz"); !ok {
		t.Error("not stored")
	}
}

func TestGitHubRetriesATruncatedBody(t *testing.T) {
	var n counter
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		if n.inc(); n.get() == 1 {
			truncate(w, []byte("ASSET BYTES"), 4)
		}
		_, _ = w.Write([]byte("ASSET BYTES"))
	})
	f := newGitHubFixture(t, up)
	r := f.get(t, "/github"+ghAsset)
	if r.code != http.StatusFound || n.get() != 2 {
		t.Fatalf("%d after %d upstream requests, want a redirect after 2", r.code, n.get())
	}
	if b, _ := f.store.object("github/octo/tool/releases/download/v1.2.3/tool-linux-amd64.tar.gz"); string(b) != "ASSET BYTES" {
		t.Errorf("stored %q", b)
	}
}

func TestGitHubGivesUpAfterTheWindow(t *testing.T) {
	var n counter
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		n.inc()
		http.Error(w, "gateway timeout", http.StatusGatewayTimeout)
	})
	f := newGitHubFixture(t, up, "-github-releases-retry-window", "100ms")
	r := f.get(t, "/github"+ghAsset)
	if r.code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502", r.code)
	}
	if n.get() < 2 || n.get() > 20 {
		t.Errorf("%d upstream requests within the window, want a handful", n.get())
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("stored %v", keys)
	}
	if !strings.Contains(f.metrics(t), `cache_redirect_requests_total{frontend="github",result="upstream_error"} 1`) {
		t.Error("upstream_error not counted")
	}
}

func TestGitHubNotFoundIsPassedOnAndNeverCached(t *testing.T) {
	var n counter
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if n.inc(); n.get() <= 2 {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("NOW IT EXISTS"))
	})
	f := newGitHubFixture(t, up)
	for i := 0; i < 2; i++ {
		if r := f.get(t, "/github"+ghAsset); r.code != 404 {
			t.Fatalf("request %d: %d, want 404", i, r.code)
		}
	}
	if n.get() != 2 {
		t.Errorf("%d upstream requests, want each 404 to ask again (and no retry of a 404)", n.get())
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("a 404 stored %v", keys)
	}
	// Once it exists it is served: nothing negative was remembered.
	if r := f.get(t, "/github"+ghAsset); r.code != http.StatusFound {
		t.Errorf("after publication: %d", r.code)
	}
}

func TestGitHubFollowsGitHubsRedirectToTheAssetHost(t *testing.T) {
	cdn := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("FROM CDN")) })
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cdn.URL+"/asset-abc?sig=1", http.StatusFound)
	})
	f := newGitHubFixture(t, up)
	if r := f.get(t, "/github"+ghAsset); r.code != http.StatusFound {
		t.Fatalf("%d", r.code)
	}
	if b, _ := f.store.object("github/octo/tool/releases/download/v1.2.3/tool-linux-amd64.tar.gz"); string(b) != "FROM CDN" {
		t.Errorf("stored %q", b)
	}
}

func TestGitHubRefusesARedirectOffGitHub(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://evil.example/steal", http.StatusFound)
	})
	f := newGitHubFixture(t, up)
	if r := f.get(t, "/github"+ghAsset); r.code != http.StatusBadGateway {
		t.Errorf("status %d, want 502", r.code)
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("stored %v", keys)
	}
	for host, want := range map[string]bool{
		"release-assets.githubusercontent.com": true, "objects.githubusercontent.com": true,
		"githubusercontent.com": true, "evilgithubusercontent.com": false, "githubusercontent.com.evil.example": false,
		"github.com": false, // not the configured upstream here
	} {
		req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/x", nil)
		got := githubRedirectCheck(mustURL(t, up.URL))(req, nil) == nil
		if got != want {
			t.Errorf("redirect to %s allowed = %v, want %v", host, got, want)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, "http://release-assets.githubusercontent.com/x", nil)
	if githubRedirectCheck(mustURL(t, "https://github.com"))(req, nil) == nil {
		t.Error("a redirect to plain HTTP was allowed")
	}
}

func TestGitHubRefusesBadPaths(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("X")) })
	f := newGitHubFixture(t, up)
	for _, bad := range []string{
		"/github",
		"/github/",
		"/github/octo/tool",
		"/github/octo/tool/releases",
		"/github/octo/tool/releases/download/v1",           // no asset
		"/github/octo/tool/releases/download/v1/",          // empty asset
		"/github/octo/tool/releases/latest/download/a.tgz", // not /releases/download/
		"/github/octo/tool/archive/refs/tags/v1.tar.gz",
		"/github/octo/tool/raw/main/file",
		"/github/octo/tool/releases/tag/v1/a",
		"/github/octo/tool/releases/download/v1/a.tgz/extra",
		"/github/octo//releases/download/v1/a.tgz",
		"/github//tool/releases/download/v1/a.tgz",
		"/github/octo/tool/releases/download//a.tgz",
		"/github/../tool/releases/download/v1/a.tgz",
		"/github/octo/tool/releases/download/../a.tgz",
		"/github/octo/tool/releases/download/v1/..",
		"/github/octo/tool/releases/download/v1/%2e%2e",
		"/github/octo/tool/releases/download/v1%2Fx/a.tgz",
		"/github/octo/tool/releases/download/v1/a%2Fb",
		"/github/octo/tool/releases/download/v1/a%3Fb=1",
		"/github/octo/tool/releases/download/v1/a%23b",
		"/github/octo/tool/releases/download/v1/a%00b",
		"/github/octo/tool/releases/download/v1/a%0Ab",
		"/github/octo/tool/releases/download/v1/a%5Cb",
		"/github/octo/tool/releases/download/v1/a%20b",
		"/github/octo/tool/releases/download/v1/a:b",
		"/github/oc_to/tool/releases/download/v1/a.tgz",
		"/github/octo/to%40ol/releases/download/v1/a.tgz",
		"/github/octo/tool/releases/download/v1/" + strings.Repeat("a", 256),
		"/github/" + strings.Repeat("o", 40) + "/tool/releases/download/v1/a.tgz",
		"/githubx/octo/tool/releases/download/v1/a.tgz",
	} {
		r := f.get(t, bad)
		// The mux itself moves a path with empty or dot elements to its
		// cleaned form (307); that is a redirect to a path of this service,
		// and the cleaned form is refused or served by the same rules.
		cleaned := r.code == http.StatusTemporaryRedirect && strings.HasPrefix(r.header.Get("Location"), "/")
		if r.code != 404 && !cleaned {
			t.Errorf("%s: %d %q, want 404", bad, r.code, r.header.Get("Location"))
		}
	}
	if up.total() != 0 {
		t.Errorf("a refused path went upstream: %v", up.paths)
	}
	if keys := f.store.keys(); len(keys) != 0 {
		t.Errorf("stored %v", keys)
	}
	// Only GET and HEAD.
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		if r := f.do(t, m, "/github"+ghAsset); r.code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d, want 405", m, r.code)
		}
	}
	// What is allowed in a tag and an asset name.
	if r := f.get(t, "/github/octo/.github/releases/download/v1.2.3+build.5/a_b-c.d.tgz"); r.code != http.StatusFound {
		t.Errorf("a plain name: %d", r.code)
	}
	if !strings.HasSuffix(up.paths[0], "/octo/.github/releases/download/v1.2.3+build.5/a_b-c.d.tgz") {
		t.Errorf("upstream asked for %v", up.paths)
	}
}

func TestGitHubSizeLimit(t *testing.T) {
	// Announced too large: refused without reading the body.
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(1000))
		w.WriteHeader(200)
		_, _ = w.Write([]byte(strings.Repeat("x", 1000)))
	})
	f := newGitHubFixture(t, up, "-github-releases-max-bytes", "100")
	if r := f.get(t, "/github"+ghAsset); r.code != http.StatusRequestEntityTooLarge {
		t.Errorf("announced: %d, want 413", r.code)
	}
	// Not announced (chunked), too large: stopped, nothing committed.
	chunked := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		fl := w.(http.Flusher)
		for i := 0; i < 20; i++ {
			_, _ = w.Write([]byte(strings.Repeat("x", 50)))
			fl.Flush()
		}
	})
	f2 := newGitHubFixture(t, chunked, "-github-releases-max-bytes", "100")
	if r := f2.get(t, "/github"+ghAsset); r.code != http.StatusRequestEntityTooLarge {
		t.Errorf("chunked: %d, want 413", r.code)
	}
	for _, f := range []*fixture{f, f2} {
		if keys := f.store.keys(); len(keys) != 0 {
			t.Errorf("stored %v", keys)
		}
		if !strings.Contains(f.metrics(t), `cache_redirect_requests_total{frontend="github",result="rejected"} 1`) {
			t.Error("rejected not counted")
		}
	}
}

func TestGitHubConcurrentMissesShareOneFetch(t *testing.T) {
	g := newGate()
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		g.wait()
		_, _ = w.Write([]byte("ASSET"))
	})
	f := newGitHubFixture(t, up)
	var followed counter
	f.cache.onFollow = followed.inc

	done := make(chan reply, 3)
	for i := 0; i < 3; i++ {
		go func() { done <- f.get(t, "/github"+ghAsset) }()
		if i == 0 {
			<-g.arrived
		}
	}
	waitFor(t, "two requests to join the fetch", func() bool { return followed.get() == 2 })
	close(g.release)
	for i := 0; i < 3; i++ {
		if r := <-done; r.code != http.StatusFound {
			t.Errorf("request %d: %d", i, r.code)
		}
	}
	if up.total() != 1 {
		t.Errorf("%d upstream fetches, want 1", up.total())
	}
}

func TestGitHubStoreFailureIs502(t *testing.T) {
	up := newUpstream(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ASSET")) })
	f := newGitHubFixture(t, up)
	f.store.err = errDenied
	if r := f.get(t, "/github"+ghAsset); r.code != http.StatusBadGateway {
		t.Errorf("status %d, want 502", r.code)
	}
}

func TestGitHubConfig(t *testing.T) {
	none := func(string) string { return "" }
	c, err := parseConfig([]string{"-s3-bucket", "b"}, none, io.Discard)
	if err != nil || c.GitHub {
		t.Fatalf("default: %v, github %v: want it off", err, c.GitHub)
	}
	c, err = parseConfig([]string{"-s3-bucket", "b", "-github-releases"}, none, io.Discard)
	if err != nil || !c.GitHub || c.GitHubPrefix != "github/" || c.GitHubUpstream != "https://github.com" ||
		c.GitHubRetryWindow != 2*time.Minute || c.GitHubMaxBytes != 2<<30 {
		t.Fatalf("defaults: %v, %+v", err, c)
	}
	env := map[string]string{"CACHE_REDIRECT_GITHUB_RELEASES": "true"}
	c, err = parseConfig([]string{"-s3-bucket", "b", "-nix=false", "-goproxy=false", "-maven=false"}, func(k string) string { return env[k] }, io.Discard)
	if err != nil || !c.GitHub {
		t.Fatalf("from the environment: %v", err)
	}
	for want, args := range map[string][]string{
		"overlap":                       {"-s3-bucket", "b", "-github-releases", "-github-releases-prefix", "nix/x/"},
		"must not be empty":             {"-s3-bucket", "b", "-github-releases", "-github-releases-prefix", ""},
		"-github-releases-upstream":     {"-s3-bucket", "b", "-github-releases", "-github-releases-upstream", "github.com"},
		"-github-releases-max-bytes":    {"-s3-bucket", "b", "-github-releases", "-github-releases-max-bytes", "0"},
		"-github-releases-retry-window": {"-s3-bucket", "b", "-github-releases", "-github-releases-retry-window", "-1s"},
	} {
		if _, err := parseConfig(args, none, io.Discard); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: err = %v, want one saying %q", args, err, want)
		}
	}
}

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
