package upgrade

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"v1.2.3", "v1.2.4", -1, true},
		{"v1.2.3", "v1.2.3", 0, true},
		{"v1.2.3", "1.2.2", 1, true},
		{"v1.10.0", "v1.9.9", 1, true}, // numeric, not lexicographic
		{"v1.0.0-3-gabc", "v1.0.0", 0, true},
		{"v2.0.0-rc.1", "v1.9.9", 1, true}, // suffix ignored
		{"dev", "v1.2.3", 0, false},
		{"ca0f6e5", "v1.2.3", 0, false},
		{"", "v1.2.3", 0, false},
		{"v1.2", "v1.2.3", 0, false},
	}
	for _, c := range cases {
		got, ok := compareSemver(c.a, c.b)
		if got != c.want || ok != c.ok {
			t.Errorf("compareSemver(%q, %q) = (%d, %v), want (%d, %v)", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}

// redirectServer serves the /releases/latest redirect the way github.com
// does: 302 with a Location header pointing at the tag page.
func redirectServer(t *testing.T, code int, location string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if location != "" {
			w.Header().Set("Location", location)
		}
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestLatestViaRedirect(t *testing.T) {
	srv := redirectServer(t, http.StatusFound, "https://github.com/PWZER/llm-switch/releases/tag/v1.2.3")
	latestPageURL = srv.URL

	rel, err := Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.TagName != "v1.2.3" {
		t.Fatalf("TagName = %q, want v1.2.3", rel.TagName)
	}
	if len(rel.Assets) != 0 {
		t.Fatalf("redirect-only release must carry no assets, got %d", len(rel.Assets))
	}
}

func TestLatestViaRedirectURLEncodedTag(t *testing.T) {
	srv := redirectServer(t, http.StatusFound, "https://github.com/PWZER/llm-switch/releases/tag/v1.2.3-rc.1")
	latestPageURL = srv.URL

	rel, err := Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.TagName != "v1.2.3-rc.1" {
		t.Fatalf("TagName = %q, want v1.2.3-rc.1", rel.TagName)
	}
}

func TestLatestFallsBackToAPI(t *testing.T) {
	var srvURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			http.NotFound(w, r) // redirect path fails…
			return
		}
		// …and the API answers, capturing the headers it was called with.
		if got := r.Header.Get("User-Agent"); got != userAgent {
			t.Errorf("User-Agent = %q, want %q", got, userAgent)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("Authorization = %q, want Bearer tok", got)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"tag_name":"v1.2.3","assets":[{"name":"llm-switch-linux-amd64","size":7,"browser_download_url":"%s/dl"}]}`, srvURL)
	}))
	srvURL = srv.URL
	t.Cleanup(srv.Close)

	latestPageURL = srv.URL + "/releases/latest"
	latestAPIURL = srv.URL + "/api"
	t.Setenv("GITHUB_TOKEN", "tok")

	rel, err := Latest()
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if rel.TagName != "v1.2.3" || len(rel.Assets) != 1 {
		t.Fatalf("unexpected release: %+v", rel)
	}
}

func TestLatestAPIRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Reset", "1700000000")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	}))
	t.Cleanup(srv.Close)

	latestPageURL = srv.URL + "/page"
	latestAPIURL = srv.URL + "/api"

	_, err := Latest()
	if err == nil {
		t.Fatal("Latest succeeded, want rate-limit error")
	}
	if !strings.Contains(err.Error(), "rate limit") || !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Fatalf("error lacks rate-limit context: %v", err)
	}
}

func TestLatestBothPathsFail(t *testing.T) {
	srv := redirectServer(t, http.StatusNotFound, "")
	latestPageURL = srv.URL + "/page"
	latestAPIURL = srv.URL + "/api"

	_, err := Latest()
	if err == nil {
		t.Fatal("Latest succeeded, want combined error")
	}
	if !strings.Contains(err.Error(), "fallback") {
		t.Fatalf("error lacks fallback context: %v", err)
	}
}

func TestMatchAsset(t *testing.T) {
	rel := &Release{TagName: "v1.0.0", Assets: []Asset{
		{Name: "llm-switch-linux-amd64", Size: 1, BrowserDownloadURL: "https://x/linux-amd64"},
		{Name: "llm-switch-darwin-arm64", Size: 2, BrowserDownloadURL: "https://x/darwin-arm64"},
	}}
	want := map[string]struct {
		url  string
		size int64
	}{
		"linux/amd64":  {"https://x/linux-amd64", 1},
		"darwin/arm64": {"https://x/darwin-arm64", 2},
	}
	for platform, exp := range want {
		goos, goarch := platform[:strings.Index(platform, "/")], platform[strings.Index(platform, "/")+1:]
		url, size, err := assetURL(rel, goos, goarch)
		if err != nil {
			t.Fatalf("assetURL(%s): %v", platform, err)
		}
		if url != exp.url || size != exp.size {
			t.Fatalf("assetURL(%s) = (%q, %d), want (%q, %d)", platform, url, size, exp.url, exp.size)
		}
	}

	// linux/arm64 is absent: error naming the expected asset.
	_, _, err := assetURL(rel, "linux", "arm64")
	if err == nil || !strings.Contains(err.Error(), "llm-switch-linux-arm64") {
		t.Fatalf("assetURL missing asset error = %v", err)
	}

	// Redirect-only resolution (no assets listed) constructs the URL.
	url, size, err := assetURL(&Release{TagName: "v9.9.9"}, "linux", "arm64")
	if err != nil || size != 0 {
		t.Fatalf("constructed assetURL = (%q, %d, %v)", url, size, err)
	}
	if !strings.Contains(url, "releases/download/v9.9.9/llm-switch-linux-arm64") {
		t.Fatalf("constructed URL = %q", url)
	}
}

func TestDownloadVerifiesContentLength(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write(make([]byte, 40)) // short write
	}))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "bin.tmp")
	err := download(dest, srv.URL, 100, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "expected") {
		t.Fatalf("download error = %v, want size mismatch", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("partial file not removed after size mismatch")
	}
}

func TestDownloadCleansUpOnPartialBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "100")
		w.Write(make([]byte, 40))
		// Hijack-style abrupt close: panic in handler after partial write is
		// noisy; instead just return with a body shorter than declared.
	}))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "bin.tmp")
	if err := download(dest, srv.URL, 0, &strings.Builder{}); err == nil {
		t.Fatal("download succeeded on truncated body, want error")
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("partial file not removed after truncated body")
	}
}

func TestDownloadSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("binary-bytes"))
	}))
	t.Cleanup(srv.Close)

	dest := filepath.Join(t.TempDir(), "bin.tmp")
	if err := download(dest, srv.URL, int64(len("binary-bytes")), &strings.Builder{}); err != nil {
		t.Fatalf("download: %v", err)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len("binary-bytes")) {
		t.Fatalf("size = %d", info.Size())
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", info.Mode().Perm())
	}
}

func TestRunCheckOnly(t *testing.T) {
	srv := redirectServer(t, http.StatusFound, "https://github.com/PWZER/llm-switch/releases/tag/v2.0.0")
	latestPageURL = srv.URL

	var out strings.Builder
	err := Run(Options{CurrentVer: "v1.0.0", CheckOnly: true, Stdout: &out, Stderr: &strings.Builder{}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "v1.0.0") || !strings.Contains(out.String(), "v2.0.0") {
		t.Fatalf("check output = %q", out.String())
	}
}

func TestRunUpToDateAndDowngrade(t *testing.T) {
	srv := redirectServer(t, http.StatusFound, "https://github.com/PWZER/llm-switch/releases/tag/v1.0.0")
	latestPageURL = srv.URL

	var out strings.Builder
	if err := Run(Options{CurrentVer: "v1.0.0", Stdout: &out, Stderr: &strings.Builder{}}); err != nil {
		t.Fatalf("Run same version: %v", err)
	}
	if !strings.Contains(out.String(), "already up to date") {
		t.Fatalf("output = %q", out.String())
	}

	out.Reset()
	if err := Run(Options{CurrentVer: "v2.0.0", Stdout: &out, Stderr: &strings.Builder{}}); err != nil {
		t.Fatalf("Run newer version: %v", err)
	}
	if !strings.Contains(out.String(), "newer than the latest release") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunUnparseableCurrentVersionWarnsAndChecks(t *testing.T) {
	srv := redirectServer(t, http.StatusFound, "https://github.com/PWZER/llm-switch/releases/tag/v1.0.0")
	latestPageURL = srv.URL

	var errOut strings.Builder
	if err := Run(Options{CurrentVer: "dev", CheckOnly: true, Stdout: &strings.Builder{}, Stderr: &errOut}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(errOut.String(), "cannot parse current version") {
		t.Fatalf("stderr = %q", errOut.String())
	}
}

func TestRunMissingAssetErrors(t *testing.T) {
	// A release whose assets do not include this platform must fail loudly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/releases/latest" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"tag_name":"v2.0.0","assets":[{"name":"llm-switch-plan9-arm","size":1,"browser_download_url":"https://x"}]}`)
	}))
	t.Cleanup(srv.Close)
	latestPageURL = srv.URL + "/releases/latest"
	latestAPIURL = srv.URL + "/api"

	err := Run(Options{CurrentVer: "v1.0.0", Yes: true, Stdout: &strings.Builder{}, Stderr: &strings.Builder{}})
	if err == nil || !strings.Contains(err.Error(), "has no asset") {
		t.Fatalf("Run error = %v, want missing-asset error", err)
	}
}
