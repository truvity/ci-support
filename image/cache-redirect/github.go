package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const frontendGitHub = "github"

// githubFrontend pulls the release assets of PUBLIC GitHub repositories
// through the cache, under /github:
//
//	/github/<owner>/<repo>/releases/download/<tag>/<asset>
//
// It answers what https://github.com/<owner>/<repo>/releases/download/<tag>/<asset>
// answers, so a downloader needs only to swap the leading https://github.com/
// for <base>/github/. A release asset is large and immutable by convention:
// a hit is a redirect to a presigned URL of the bucket and never asks GitHub.
// A miss is fetched whole (GitHub's own redirect to its asset host is
// followed), asked again for a couple of minutes while GitHub's edge answers
// 5xx or times out, committed to the bucket, and only then answered with the
// same redirect: a client never sees half an asset, and never sees GitHub's
// 504. A 404 is the client's 404 and is never stored.
//
// The upstream is fixed and carries no credential, so it is not an open
// proxy: only these six path elements, each from a narrow character set,
// are ever turned into a request, and a redirect off GitHub's own hosts is
// refused. Nothing but a release asset's download path is served.
type githubFrontend struct {
	cache    *Cache
	prefix   string
	upstream *url.URL
	maxBytes int64
}

func (g *githubFrontend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !getOrHead(w, r) {
		return
	}
	owner, repo, tag, asset, ok := parseGitHubReleasePath(strings.TrimPrefix(r.URL.Path, "/github"))
	if !ok {
		notFoundHere(w, g.cache.metrics, frontendGitHub)
		return
	}
	// GitHub names are case-insensitive for owner and repository: one object
	// per repository, whatever the case of the request. The tag and the asset
	// name are case-sensitive.
	rel := owner + "/" + repo + "/releases/download/" + tag + "/" + asset
	g.cache.ServeImmutable(w, r, object{
		frontend: frontendGitHub, key: g.prefix + strings.ToLower(owner) + "/" + strings.ToLower(repo) + "/releases/download/" + tag + "/" + asset,
		upstream: joinURL(g.upstream, "/"+rel), contentType: "application/octet-stream", large: true,
		storeThenRedirect: true, noNegative: true, maxBytes: g.maxBytes,
	})
}

// parseGitHubReleasePath takes "/<owner>/<repo>/releases/download/<tag>/<asset>"
// and nothing else. The path is already decoded, so a "%2F" has become a
// separator and fails the element count, and every element is held to a
// character set that has no separator, no escape and no way out of the
// prefix.
func parseGitHubReleasePath(p string) (owner, repo, tag, asset string, ok bool) {
	if !strings.HasPrefix(p, "/") {
		return
	}
	els := strings.Split(p[1:], "/")
	if len(els) != 6 || els[2] != "releases" || els[3] != "download" {
		return
	}
	owner, repo, tag, asset = els[0], els[1], els[4], els[5]
	if !validGitHubName(owner, 39, "-") || !validGitHubName(repo, 100, "._-") ||
		!validGitHubName(tag, 255, "._+-") || !validGitHubName(asset, 255, "._+-") {
		return "", "", "", "", false
	}
	return owner, repo, tag, asset, true
}

// validGitHubName admits letters, digits and the given punctuation, up to maxLen
// bytes; never empty, never "." or "..".
func validGitHubName(s string, maxLen int, punct string) bool {
	if s == "" || len(s) > maxLen || s == "." || s == ".." {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isAlnum(s[i]) && strings.IndexByte(punct, s[i]) < 0 {
			return false
		}
	}
	return true
}

// errRedirectRefused is a redirect the policy refused: the upstream will
// answer the same way again, so it is not retried.
var errRedirectRefused = errors.New("redirect refused")

// githubRedirectCheck is the redirect policy of the GitHub front-end: GitHub
// answers a release download with a redirect to its asset host
// (release-assets.githubusercontent.com, formerly objects.githubusercontent.com),
// and nowhere else may it lead. The configured upstream's own host is allowed
// too (a mirror, or a test server).
func githubRedirectCheck(upstream *url.URL) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("%w: too many redirects", errRedirectRefused)
		}
		if req.URL.Scheme != "https" && upstream.Scheme != "http" {
			return fmt.Errorf("%w: to a plain-HTTP URL", errRedirectRefused)
		}
		h := normalizeHost(req.URL.Host)
		if h == normalizeHost(upstream.Host) || h == "githubusercontent.com" || strings.HasSuffix(h, ".githubusercontent.com") {
			return nil
		}
		return fmt.Errorf("%w: to a host that is not GitHub's", errRedirectRefused)
	}
}
