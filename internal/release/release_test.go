package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func new_test_server(t *testing.T, version string, binary []byte) *httptest.Server {
	sum := sha256.Sum256(binary)
	manifest := Manifest{
		Version: version,
		Platforms: map[Platform]ManifestEntry{
			"linux-x64": {
				Binary:   "claude",
				Checksum: hex.EncodeToString(sum[:]),
				Size:     int64(len(binary)),
			},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/stable", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, version)
	})
	mux.HandleFunc(fmt.Sprintf("/%s/manifest.json", version), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(manifest)
	})
	mux.HandleFunc(fmt.Sprintf("/%s/linux-x64/claude", version), func(w http.ResponseWriter, r *http.Request) {
		w.Write(binary)
	})

	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestPlatformFor(t *testing.T) {
	t.Run("glibc", func(t *testing.T) {
		p, err := PlatformFor("amd64", false)
		require.NoError(t, err)
		require.Equal(t, Platform("linux-x64"), p)
	})
	t.Run("musl arm64", func(t *testing.T) {
		p, err := PlatformFor("aarch64", true)
		require.NoError(t, err)
		require.Equal(t, Platform("linux-arm64-musl"), p)
	})
	t.Run("unsupported", func(t *testing.T) {
		_, err := PlatformFor("riscv64", false)
		require.Error(t, err)
	})
}

func TestCacheEnsure(t *testing.T) {
	t.Run("download, verify, and reuse", func(t *testing.T) {
		binary := []byte("#!/bin/sh\necho claude\n")
		s := new_test_server(t, "1.2.3", binary)

		cache := &Cache{Dir: t.TempDir(), Client: NewClient(s.URL)}
		p, err := cache.Ensure(context.Background(), "1.2.3", "linux-x64")
		require.NoError(t, err)

		data, err := os.ReadFile(p)
		require.NoError(t, err)
		require.Equal(t, binary, data)

		fi, err := os.Stat(p)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o755), fi.Mode().Perm())

		// A second call must not hit the network.
		s.Close()
		p2, err := cache.Ensure(context.Background(), "1.2.3", "linux-x64")
		require.NoError(t, err)
		require.Equal(t, p, p2)
	})
	t.Run("checksum mismatch rejects the download", func(t *testing.T) {
		// The manifest promises "legit" but the download serves "tampered".
		sum := sha256.Sum256([]byte("legit"))
		mux := http.NewServeMux()
		mux.HandleFunc("/1.2.3/manifest.json", func(w http.ResponseWriter, r *http.Request) {
			json.NewEncoder(w).Encode(Manifest{
				Version: "1.2.3",
				Platforms: map[Platform]ManifestEntry{
					"linux-x64": {Checksum: hex.EncodeToString(sum[:])},
				},
			})
		})
		mux.HandleFunc("/1.2.3/linux-x64/claude", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("tampered"))
		})
		s := httptest.NewServer(mux)
		t.Cleanup(s.Close)

		cache := &Cache{Dir: t.TempDir(), Client: NewClient(s.URL)}
		_, err := cache.Ensure(context.Background(), "1.2.3", "linux-x64")
		require.ErrorContains(t, err, "checksum mismatch")

		_, err = os.Stat(cache.Path("1.2.3", "linux-x64"))
		require.True(t, os.IsNotExist(err))
	})
}

func TestCacheGC(t *testing.T) {
	dir := t.TempDir()
	cache := &Cache{Dir: dir}
	for _, v := range []string{"1.0.0", "1.1.0", "2.0.0"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, v, "linux-x64"), 0o755))
	}

	require.NoError(t, cache.GC("1.1.0"))
	require.ElementsMatch(t, []string{"1.1.0"}, cache.Versions())
}

func TestCompareVersions(t *testing.T) {
	t.Run("ordering", func(t *testing.T) {
		require.Equal(t, 1, compare_versions("2.1.10", "2.1.9"))
		require.Equal(t, -1, compare_versions("2.1.9", "2.2.0"))
		require.Equal(t, 0, compare_versions("1.2.3", "1.2.3"))
	})
	t.Run("pick newest", func(t *testing.T) {
		require.Equal(t, "2.1.10", pick_newest([]string{"2.1.9", "2.1.10", "1.9.9"}))
	})
}
