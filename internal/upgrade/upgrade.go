// Package upgrade implements `llm-switch upgrade`: resolve the latest
// GitHub release, download the platform asset, atomically replace the
// running binary, and restart a running instance with its original flags.
//
// Release assets are the raw cross-compiled binaries named
// llm-switch-<goos>-<goarch> (see the Makefile release target); the lookup
// below matches that naming exactly.
package upgrade

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/PWZER/llm-switch/internal/daemon"
)

const (
	userAgent     = "llm-switch-upgrade"
	assetNameFmt  = "llm-switch-%s-%s"
	restartGrace  = 45 * time.Second // 30s HTTP drain + 10s stats drain in run()
	confirmPrompt = "proceed"
)

// Overridable in tests so the GitHub endpoints can point at httptest servers.
var (
	latestPageURL  = "https://github.com/PWZER/llm-switch/releases/latest"
	latestAPIURL   = "https://api.github.com/repos/PWZER/llm-switch/releases/latest"
	downloadURLFmt = "https://github.com/PWZER/llm-switch/releases/download/%s/" + assetNameFmt
)

// Asset mirrors one release asset of the GitHub releases API.
type Asset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// Release mirrors the subset of the GitHub releases API response used here.
// A tag resolved via the /releases/latest redirect carries no assets — the
// download URL is then constructed from the tag.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Latest resolves the most recent published release: first via the
// /releases/latest redirect (no API rate limit applies), falling back to the
// REST API (which honors GITHUB_TOKEN / GH_TOKEN).
func Latest() (*Release, error) {
	tag, err := latestViaRedirect()
	if err == nil {
		return &Release{TagName: tag}, nil
	}
	redirectErr := err
	rel, err := latestViaAPI()
	if err != nil {
		return nil, fmt.Errorf("cannot determine the latest release: %v; API fallback also failed: %w", redirectErr, err)
	}
	return rel, nil
}

// latestViaRedirect reads the tag out of the 3xx Location of
// /releases/latest without following the redirect.
func latestViaRedirect() (string, error) {
	req, err := http.NewRequest(http.MethodGet, latestPageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := pageClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	loc := resp.Header.Get("Location")
	const marker = "/releases/tag/"
	if resp.StatusCode < 300 || resp.StatusCode >= 400 || !strings.Contains(loc, marker) {
		return "", fmt.Errorf("unexpected response %s from %s (Location: %q)", resp.Status, latestPageURL, loc)
	}
	tag, err := url.PathUnescape(loc[strings.Index(loc, marker)+len(marker):])
	if err != nil {
		return "", fmt.Errorf("parse redirect location %q: %w", loc, err)
	}
	if tag == "" || strings.Contains(tag, "/") {
		return "", fmt.Errorf("no tag in redirect location %q", loc)
	}
	return tag, nil
}

// latestViaAPI fetches the latest release from the REST API. A Bearer token
// from GITHUB_TOKEN or GH_TOKEN raises the 60 req/h anonymous rate limit.
func latestViaAPI() (*Release, error) {
	req, err := http.NewRequest(http.MethodGet, latestAPIURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/vnd.github+json")
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	} else if token := os.Getenv("GH_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp)
	}
	var rel Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decode %s response: %w", latestAPIURL, err)
	}
	if rel.TagName == "" {
		return nil, fmt.Errorf("%s returned no tag_name", latestAPIURL)
	}
	return &rel, nil
}

// apiError renders a non-200 API response, with rate-limit context on 403.
func apiError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	msg := strings.TrimSpace(string(b))
	if resp.StatusCode == http.StatusForbidden && strings.Contains(msg, "rate limit") {
		if reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			return fmt.Errorf("GitHub API rate limit exceeded (resets at %s); set GITHUB_TOKEN to raise it: %s",
				time.Unix(reset, 0).UTC().Format(time.RFC3339), msg)
		}
		return fmt.Errorf("GitHub API rate limit exceeded; set GITHUB_TOKEN to raise it: %s", msg)
	}
	if msg == "" {
		return fmt.Errorf("GitHub API returned %s", resp.Status)
	}
	return fmt.Errorf("GitHub API returned %s: %s", resp.Status, msg)
}

var verRe = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// compareSemver compares the leading numeric triple of a and b ("v" prefix
// optional; suffixes such as "-3-gabc" are ignored). ok is false when either
// side carries no X.Y.Z prefix (e.g. "dev" or a bare commit SHA) — callers
// should warn and proceed.
func compareSemver(a, b string) (int, bool) {
	ma := verRe.FindStringSubmatch(a)
	mb := verRe.FindStringSubmatch(b)
	if ma == nil || mb == nil {
		return 0, false
	}
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(ma[i])
		y, _ := strconv.Atoi(mb[i])
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// assetURL returns the download URL and expected size of this platform's
// asset. When the release lists assets, the name must match exactly — a
// mismatch means the release was built for different platforms and the error
// names the expected asset. Redirect-only resolution (no assets listed)
// constructs the releases/download URL instead.
func assetURL(rel *Release, goos, goarch string) (string, int64, error) {
	want := fmt.Sprintf(assetNameFmt, goos, goarch)
	if len(rel.Assets) == 0 {
		return fmt.Sprintf(downloadURLFmt, rel.TagName, goos, goarch), 0, nil
	}
	for _, a := range rel.Assets {
		if a.Name == want {
			if a.BrowserDownloadURL != "" {
				return a.BrowserDownloadURL, a.Size, nil
			}
			return fmt.Sprintf(downloadURLFmt, rel.TagName, goos, goarch), a.Size, nil
		}
	}
	return "", 0, fmt.Errorf("release %s has no asset %q (this build targets %s/%s)", rel.TagName, want, goos, goarch)
}

// progressWriter prints a throttled download progress line to stderr.
type progressWriter struct {
	w         io.Writer
	total     int64
	written   int64
	lastStep  int
	totalKnown bool
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.written += int64(len(b))
	if p.totalKnown {
		pct := int(p.written * 100 / p.total)
		if step := pct / 2; step != p.lastStep || pct == 100 {
			p.lastStep = step
			fmt.Fprintf(p.w, "\rdownloading: %5.1f/%5.1f MB (%2d%%)", mb(p.written), mb(p.total), pct)
		}
	} else if step := int(mb(p.written)); step != p.lastStep {
		p.lastStep = step
		fmt.Fprintf(p.w, "\rdownloading: %5.1f MB", mb(p.written))
	}
	return len(b), nil
}

func (p *progressWriter) done() {
	fmt.Fprintln(p.w)
}

func mb(n int64) float64 {
	return float64(n) / (1 << 20)
}

// download streams url into dest, verifying the transferred size against
// total (when known; falls back to Content-Length). The partial file is
// removed on any failure.
func download(dest, src string, total int64, stderr io.Writer) (err error) {
	req, err := http.NewRequest(http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := downloadClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: %s", resp.Status)
	}
	if total == 0 {
		total = resp.ContentLength
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	pw := &progressWriter{w: stderr, total: total, totalKnown: total > 0}
	_, copyErr := io.Copy(f, io.TeeReader(resp.Body, pw))
	closeErr := f.Close()
	if copyErr != nil {
		err = copyErr
	} else if closeErr != nil {
		err = closeErr
	} else if total > 0 && pw.written != total {
		err = fmt.Errorf("downloaded %d bytes, expected %d", pw.written, total)
	}
	if err != nil {
		_ = os.Remove(dest)
		return err
	}
	pw.done()
	return os.Chmod(dest, 0o755)
}

// Options configures one upgrade run. Nil writers default to os.Stdin/os.Stdout/os.Stderr.
type Options struct {
	// DataDir locates the running instance for the post-upgrade restart.
	DataDir string
	// CurrentVer is the version this binary was stamped with.
	CurrentVer string
	// CheckOnly prints the current and latest versions and returns.
	CheckOnly bool
	// Yes skips the confirmation prompt.
	Yes bool
	// Stdin/Stdout/Stderr are the IO streams (injectable for tests).
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// Run executes the upgrade flow end to end.
func Run(opts Options) error {
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}

	rel, err := Latest()
	if err != nil {
		return err
	}
	cmp, ok := compareSemver(opts.CurrentVer, rel.TagName)
	if !ok {
		fmt.Fprintf(opts.Stderr, "warning: cannot parse current version %q; comparing against %s anyway\n",
			opts.CurrentVer, rel.TagName)
	} else if cmp == 0 {
		fmt.Fprintf(opts.Stdout, "already up to date (%s)\n", opts.CurrentVer)
		return nil
	} else if cmp > 0 {
		fmt.Fprintf(opts.Stdout, "current version %s is newer than the latest release %s; nothing to do\n",
			opts.CurrentVer, rel.TagName)
		return nil
	}
	fmt.Fprintf(opts.Stdout, "current version: %s\nlatest version:  %s\n", opts.CurrentVer, rel.TagName)
	if opts.CheckOnly {
		return nil
	}

	src, size, err := assetURL(rel, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	if !opts.Yes {
		fmt.Fprintf(opts.Stdout, "upgrade llm-switch %s -> %s? [%s/N] ", opts.CurrentVer, rel.TagName, confirmPrompt)
		if !confirm(opts.Stdin) {
			fmt.Fprintln(opts.Stdout, "aborted")
			return nil
		}
	}

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current binary: %w", err)
	}
	tmp := filepath.Join(filepath.Dir(exe), "."+filepath.Base(exe)+".tmp")
	if err := download(tmp, src, size, opts.Stderr); err != nil {
		return err
	}
	if err := os.Rename(tmp, exe); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	fmt.Fprintf(opts.Stdout, "upgraded llm-switch to %s\n", rel.TagName)
	return restartDaemon(opts)
}

// confirm reads one line and accepts y/yes (case-insensitive). EOF or any
// other answer aborts.
func confirm(r io.Reader) bool {
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// restartDaemon stops a running instance and starts the freshly replaced
// binary detached with its original flags. It is a no-op when nothing runs.
func restartDaemon(opts Options) error {
	meta, ok := daemon.ReadRunMeta(opts.DataDir)
	pid := 0
	if ok && daemon.Alive(meta.PID) {
		pid = meta.PID
	} else if candidate := daemon.ReadPid(opts.DataDir); daemon.Alive(candidate) {
		pid = candidate
	}
	if pid <= 0 {
		fmt.Fprintln(opts.Stderr, "no running instance detected; start llm-switch to use the new version")
		return nil
	}
	note := ""
	if !ok {
		note = " (no run metadata found; restarting with default flags)"
	}
	fmt.Fprintf(opts.Stderr, "stopping running instance (pid %d)%s...\n", pid, note)

	err := daemon.Stop(opts.DataDir, restartGrace, false)
	switch {
	case err == nil:
	case errors.Is(err, daemon.ErrNotRunning):
		// The instance exited while we were upgrading; just start the new one.
	default:
		return fmt.Errorf("old instance (pid %d) could not be stopped: %w — the binary was already replaced; stop it and start llm-switch manually", pid, err)
	}
	if code := daemon.SpawnArgs(opts.DataDir, meta.Args); code != 0 {
		return fmt.Errorf("restarted instance failed to start; see %s", filepath.Join(opts.DataDir, daemon.LogFile))
	}
	fmt.Fprintf(opts.Stderr, "llm-switch restarted detached; logs: %s\n", filepath.Join(opts.DataDir, daemon.LogFile))
	return nil
}

// HTTP clients. Metadata calls are bounded; the download client deliberately
// has no overall timeout (release assets over slow links must not be cut
// off) — the transport bounds the connect/handshake/header phases instead.
var (
	transport = func() *http.Transport {
		return &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		}
	}
	pageClient     = &http.Client{Transport: transport(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	apiClient      = &http.Client{Transport: transport(), Timeout: 15 * time.Second}
	downloadClient = &http.Client{Transport: transport()}
)
