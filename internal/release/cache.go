package release

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sync/singleflight"
)

// Cache is an on-disk cache of claude binaries at <dir>/<version>/<platform>/claude.
type Cache struct {
	Dir    string
	Client *Client

	group singleflight.Group
}

func (c *Cache) Path(version string, p Platform) string {
	return filepath.Join(c.Dir, version, string(p), "claude")
}

// Ensure returns the path of the cached binary for (version, platform),
// downloading and sha256-verifying it first if missing. Concurrent calls
// for the same key share a single download.
func (c *Cache) Ensure(ctx context.Context, version string, p Platform) (string, error) {
	dst := c.Path(version, p)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}

	key := version + "/" + string(p)
	_, err, _ := c.group.Do(key, func() (any, error) {
		if _, err := os.Stat(dst); err == nil {
			return nil, nil
		}
		return nil, c.download(ctx, version, p, dst)
	})
	if err != nil {
		return "", err
	}
	return dst, nil
}

func (c *Cache) download(ctx context.Context, version string, p Platform, dst string) error {
	m, err := c.Client.Manifest(ctx, version)
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	entry, ok := m.Platforms[p]
	if !ok {
		return fmt.Errorf("version %s has no build for %s", version, p)
	}

	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}

	f, err := os.CreateTemp(filepath.Dir(dst), ".claude-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())

	sum, err := c.Client.Download(ctx, version, p, f)
	if err != nil {
		f.Close()
		return fmt.Errorf("download: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if sum != entry.Checksum {
		return fmt.Errorf("checksum mismatch for %s/%s: got %s want %s", version, p, sum, entry.Checksum)
	}

	if err := os.Chmod(f.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(f.Name(), dst)
}

// GC removes every cached version except keep.
// Never call it before the kept version is fully downloaded.
func (c *Cache) GC(keep string) error {
	entries, err := os.ReadDir(c.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	for _, e := range entries {
		if !e.IsDir() || e.Name() == keep {
			continue
		}
		if err := os.RemoveAll(filepath.Join(c.Dir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Versions lists cached versions, unordered.
func (c *Cache) Versions() []string {
	entries, err := os.ReadDir(c.Dir)
	if err != nil {
		return nil
	}

	vs := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			vs = append(vs, e.Name())
		}
	}
	return vs
}
