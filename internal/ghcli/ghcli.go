// Package ghcli downloads GitHub CLI (`gh`) release binaries from GitHub and
// caches them on disk keyed by (version, arch), so cld can inject `gh` into a
// devcontainer whose workspace has a GitHub remote — the same way it injects
// the claude binary.
package ghcli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

const (
	latestReleaseAPI = "https://api.github.com/repos/cli/cli/releases/latest"
	downloadBase     = "https://github.com/cli/cli/releases/download"
)

// Arch is a gh release architecture: "amd64" or "arm64". gh ships static
// binaries, so libc (musl vs glibc) does not matter — only the CPU arch does.
type Arch string

// ArchFor maps a container architecture to a gh release architecture.
func ArchFor(arch string) (Arch, error) {
	switch arch {
	case "amd64", "x86_64":
		return "amd64", nil
	case "arm64", "aarch64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported architecture: %q", arch)
	}
}

// Fetcher resolves the latest gh version and serves gh binaries from an on-disk
// cache at <Dir>/<version>/<arch>/gh, downloading and extracting the official
// GitHub release tarball on a miss. The resolved version is remembered for the
// process; when GitHub is unreachable it falls back to the newest cached
// version so provisioning keeps working offline.
type Fetcher struct {
	// Dir is the cache root; binaries live at <Dir>/<version>/<arch>/gh.
	Dir string
	// HC is the HTTP client; a 10-minute-timeout client is used when nil.
	HC *http.Client
	// BaseURL overrides the GitHub download host (tests point it at httptest);
	// empty means the real github.com. The latest-release API is derived from
	// it too so a test never reaches out to the network.
	BaseURL string

	group   singleflight.Group
	mu      sync.Mutex
	version string
	gcDone  string // last version the cache was swept down to
}

func (f *Fetcher) client() *http.Client {
	if f.HC != nil {
		return f.HC
	}
	return &http.Client{Timeout: 10 * time.Minute}
}

func (f *Fetcher) latestURL() string {
	if f.BaseURL != "" {
		return strings.TrimSuffix(f.BaseURL, "/") + "/latest"
	}
	return latestReleaseAPI
}

func (f *Fetcher) downloadBase() string {
	if f.BaseURL != "" {
		return strings.TrimSuffix(f.BaseURL, "/") + "/download"
	}
	return downloadBase
}

// Ensure returns the version and path of the cached gh binary for arch,
// resolving the latest version and downloading it first if needed. Concurrent
// calls for the same (version, arch) share a single download. Once the current
// version is present, older cached versions are garbage-collected.
func (f *Fetcher) Ensure(ctx context.Context, arch Arch) (string, string, error) {
	v, err := f.Version(ctx)
	if err != nil {
		return "", "", err
	}

	dst := f.path(v, arch)
	if _, err := os.Stat(dst); err != nil {
		key := v + "/" + string(arch)
		_, err, _ = f.group.Do(key, func() (any, error) {
			if _, err := os.Stat(dst); err == nil {
				return nil, nil
			}
			return nil, f.download(ctx, v, arch, dst)
		})
		if err != nil {
			return "", "", err
		}
	}

	// Sweep old versions once the current one is on disk. Every provision in a
	// process resolves the SAME version, so keeping v never races a concurrent
	// download of a different version. Runs even on a cache hit so versions left
	// by an earlier process are cleaned too. Best-effort: a failed sweep leaves
	// gcDone unset so it simply retries next time.
	f.gc(v)

	return v, dst, nil
}

// gc removes every cached version directory except keep, at most once per
// resolved version. Only ever called after keep is fully downloaded.
func (f *Fetcher) gc(keep string) {
	f.mu.Lock()
	done := f.gcDone == keep
	f.mu.Unlock()
	if done {
		return
	}

	entries, err := os.ReadDir(f.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(f.Dir, e.Name())); err != nil {
			return // leave gcDone unset; retry on the next Ensure
		}
	}

	f.mu.Lock()
	f.gcDone = keep
	f.mu.Unlock()
}

// Version resolves the latest gh version once and caches it for the process.
// On a network failure it falls back to the newest already-cached version.
func (f *Fetcher) Version(ctx context.Context) (string, error) {
	f.mu.Lock()
	v := f.version
	f.mu.Unlock()
	if v != "" {
		return v, nil
	}

	v, err := f.resolveLatest(ctx)
	if err != nil {
		if cached := f.newestCached(); cached != "" {
			v = cached
		} else {
			return "", err
		}
	}

	f.mu.Lock()
	f.version = v
	f.mu.Unlock()
	return v, nil
}

func (f *Fetcher) resolveLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.latestURL(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	res, err := f.client().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolve latest gh release: %s", res.Status)
	}

	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return "", err
	}
	v := strings.TrimPrefix(strings.TrimSpace(body.TagName), "v")
	if v == "" {
		return "", fmt.Errorf("latest gh release has no tag_name")
	}
	return v, nil
}

func (f *Fetcher) path(version string, arch Arch) string {
	return filepath.Join(f.Dir, version, string(arch), "gh")
}

// newestCached returns the newest cached version directory, or "" if none.
func (f *Fetcher) newestCached() string {
	entries, err := os.ReadDir(f.Dir)
	if err != nil {
		return ""
	}
	best := ""
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if best == "" || compareVersions(e.Name(), best) > 0 {
			best = e.Name()
		}
	}
	return best
}

// download fetches the release tarball for (version, arch), verifies its
// sha256 against the release checksums file, extracts the gh binary, and
// atomically renames it into place at dst.
func (f *Fetcher) download(ctx context.Context, version string, arch Arch, dst string) error {
	asset := fmt.Sprintf("gh_%s_linux_%s.tar.gz", version, arch)

	want, err := f.checksum(ctx, version, asset)
	if err != nil {
		return fmt.Errorf("checksums: %w", err)
	}

	url := fmt.Sprintf("%s/v%s/%s", f.downloadBase(), version, asset)
	res, err := f.get(ctx, url)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	// Stage the tarball to a temp file so its full-file sha256 can be verified
	// before anything is extracted.
	tgz, err := os.CreateTemp(filepath.Dir(dst), ".gh-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tgz.Name())

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tgz, h), res.Body); err != nil {
		tgz.Close()
		return fmt.Errorf("download %s: %w", asset, err)
	}
	if err := tgz.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s want %s", asset, got, want)
	}

	return extractGh(tgz.Name(), dst)
}

// checksum fetches the release's checksums file and returns the hex sha256 for
// the named asset.
func (f *Fetcher) checksum(ctx context.Context, version, asset string) (string, error) {
	url := fmt.Sprintf("%s/v%s/gh_%s_checksums.txt", f.downloadBase(), version, version)
	res, err := f.get(ctx, url)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == asset {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum for %s", asset)
}

func (f *Fetcher) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := f.client().Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, res.Status)
	}
	return res, nil
}

// extractGh reads the gh release tarball at tgzPath, extracts the `bin/gh`
// entry to a temp file, and atomically renames it to dst with 0755 mode.
func extractGh(tgzPath, dst string) error {
	f, err := os.Open(tgzPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("gh binary not found in archive")
		}
		if err != nil {
			return err
		}
		// The archive lays the binary out as gh_<ver>_linux_<arch>/bin/gh.
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != "gh" ||
			!strings.HasSuffix(hdr.Name, "/bin/gh") {
			continue
		}

		out, err := os.CreateTemp(filepath.Dir(dst), ".gh-*")
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			os.Remove(out.Name())
			return err
		}
		if err := out.Close(); err != nil {
			os.Remove(out.Name())
			return err
		}
		if err := os.Chmod(out.Name(), 0o755); err != nil {
			os.Remove(out.Name())
			return err
		}
		return os.Rename(out.Name(), dst)
	}
}

// compareVersions orders two semver-ish "x.y.z" strings; missing parts read as 0.
func compareVersions(a, b string) int {
	pa, pb := parseVersion(a), parseVersion(b)
	for i := range 3 {
		if pa[i] != pb[i] {
			if pa[i] < pb[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func parseVersion(v string) [3]int {
	var out [3]int
	i, n := 0, 0
	for _, c := range v {
		switch {
		case c >= '0' && c <= '9':
			n = n*10 + int(c-'0')
		case c == '.' && i < 2:
			out[i] = n
			n = 0
			i++
		default:
			out[i] = n
			return out
		}
	}
	out[i] = n
	return out
}
