// Package release downloads Claude Code binaries from the official release
// channel — the same HTTP API the install script uses — and caches them
// on disk keyed by (version, platform).
package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Platform is a release platform key such as "linux-x64" or "linux-arm64-musl".
type Platform string

// PlatformFor maps a container architecture and libc to a release platform.
func PlatformFor(arch string, musl bool) (Platform, error) {
	var p string
	switch arch {
	case "amd64", "x86_64":
		p = "linux-x64"
	case "arm64", "aarch64":
		p = "linux-arm64"
	default:
		return "", fmt.Errorf("unsupported architecture: %q", arch)
	}
	if musl {
		p += "-musl"
	}
	return Platform(p), nil
}

type Manifest struct {
	Version   string                     `json:"version"`
	Platforms map[Platform]ManifestEntry `json:"platforms"`
}

type ManifestEntry struct {
	Binary   string `json:"binary"`
	Checksum string `json:"checksum"`
	Size     int64  `json:"size"`
}

type Client struct {
	base string
	hc   *http.Client
}

func NewClient(base string) *Client {
	return &Client{
		base: strings.TrimSuffix(base, "/"),
		hc:   &http.Client{Timeout: 10 * time.Minute},
	}
}

func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}

	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", path, res.Status)
	}
	return res, nil
}

// Version resolves the current version of a channel ("stable" or "latest").
func (c *Client) Version(ctx context.Context, channel string) (string, error) {
	res, err := c.get(ctx, "/"+channel)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	v, err := io.ReadAll(io.LimitReader(res.Body, 128))
	if err != nil {
		return "", err
	}

	s := strings.TrimSpace(string(v))
	if s == "" {
		return "", fmt.Errorf("channel %q resolved to an empty version", channel)
	}
	return s, nil
}

func (c *Client) Manifest(ctx context.Context, version string) (*Manifest, error) {
	res, err := c.get(ctx, "/"+version+"/manifest.json")
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	var m Manifest
	if err := json.NewDecoder(res.Body).Decode(&m); err != nil {
		return nil, err
	}
	return &m, nil
}

// Download streams a binary into w and returns the hex sha256 of the bytes written.
func (c *Client) Download(ctx context.Context, version string, p Platform, w io.Writer) (string, error) {
	res, err := c.get(ctx, fmt.Sprintf("/%s/%s/claude", version, p))
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(w, h), res.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
