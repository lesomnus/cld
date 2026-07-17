package ghcli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// buildTarball returns a gh-style release tarball carrying bin at the archive's
// gh_<version>_linux_<arch>/bin/gh path, plus its hex sha256.
func buildTarball(t *testing.T, version string, arch Arch, bin []byte) ([]byte, string) {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	name := fmt.Sprintf("gh_%s_linux_%s/bin/gh", version, arch)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o755,
		Size:     int64(len(bin)),
		Typeflag: tar.TypeReg,
	}))
	_, err := tw.Write(bin)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())

	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), hex.EncodeToString(sum[:])
}

func newTestServer(t *testing.T, version string, arch Arch, bin []byte) *httptest.Server {
	t.Helper()

	tgz, sum := buildTarball(t, version, arch, bin)
	asset := fmt.Sprintf("gh_%s_linux_%s.tar.gz", version, arch)

	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v%s"}`, version)
	})
	mux.HandleFunc(fmt.Sprintf("/download/v%s/gh_%s_checksums.txt", version, version),
		func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s  %s\n", sum, asset)
		})
	mux.HandleFunc(fmt.Sprintf("/download/v%s/%s", version, asset),
		func(w http.ResponseWriter, r *http.Request) {
			w.Write(tgz)
		})
	return httptest.NewServer(mux)
}

func TestEnsureDownloadsAndExtracts(t *testing.T) {
	require := require.New(t)

	bin := []byte("#!/bin/sh\necho gh\n")
	srv := newTestServer(t, "2.62.0", "amd64", bin)
	defer srv.Close()

	f := &Fetcher{Dir: t.TempDir(), BaseURL: srv.URL}
	version, path, err := f.Ensure(context.Background(), "amd64")
	require.NoError(err)
	require.Equal("2.62.0", version)

	got, err := os.ReadFile(path)
	require.NoError(err)
	require.Equal(bin, got)

	info, err := os.Stat(path)
	require.NoError(err)
	require.Equal(os.FileMode(0o755), info.Mode().Perm())
}

func TestEnsureIsCachedOnSecondCall(t *testing.T) {
	require := require.New(t)

	bin := []byte("gh-binary")
	srv := newTestServer(t, "2.62.0", "amd64", bin)

	f := &Fetcher{Dir: t.TempDir(), BaseURL: srv.URL}
	_, path1, err := f.Ensure(context.Background(), "amd64")
	require.NoError(err)

	// Close the server: a cached hit must not touch the network.
	srv.Close()
	_, path2, err := f.Ensure(context.Background(), "amd64")
	require.NoError(err)
	require.Equal(path1, path2)
}

func TestEnsureGCsOldVersions(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	// A stale version left by an earlier run, plus a junk file that must survive.
	require.NoError(os.MkdirAll(dir+"/2.60.0/amd64", 0o755))
	require.NoError(os.WriteFile(dir+"/2.60.0/amd64/gh", []byte("old"), 0o755))
	require.NoError(os.WriteFile(dir+"/notes.txt", []byte("keep me"), 0o644))

	srv := newTestServer(t, "2.62.0", "amd64", []byte("gh-binary"))
	defer srv.Close()

	f := &Fetcher{Dir: dir, BaseURL: srv.URL}
	_, _, err := f.Ensure(context.Background(), "amd64")
	require.NoError(err)

	// Old version swept, current version kept, non-dir entry untouched.
	require.NoDirExists(dir + "/2.60.0")
	require.DirExists(dir + "/2.62.0/amd64")
	require.FileExists(dir + "/notes.txt")
}

func TestVersionFallsBackToCacheOffline(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	// Seed a cached version directory so the offline fallback has something.
	require.NoError(os.MkdirAll(dir+"/2.60.0/amd64", 0o755))

	// BaseURL points nowhere reachable; resolveLatest fails.
	f := &Fetcher{Dir: dir, BaseURL: "http://127.0.0.1:0"}
	v, err := f.Version(context.Background())
	require.NoError(err)
	require.Equal("2.60.0", v)
}

func TestChecksumMismatchFails(t *testing.T) {
	require := require.New(t)

	version, arch := "2.62.0", Arch("amd64")
	tgz, _ := buildTarball(t, version, arch, []byte("real"))
	asset := fmt.Sprintf("gh_%s_linux_%s.tar.gz", version, arch)

	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"tag_name":"v%s"}`, version)
	})
	mux.HandleFunc(fmt.Sprintf("/download/v%s/gh_%s_checksums.txt", version, version),
		func(w http.ResponseWriter, r *http.Request) {
			// Wrong checksum for the asset.
			fmt.Fprintf(w, "%s  %s\n", "deadbeef", asset)
		})
	mux.HandleFunc(fmt.Sprintf("/download/v%s/%s", version, asset),
		func(w http.ResponseWriter, r *http.Request) { w.Write(tgz) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	f := &Fetcher{Dir: t.TempDir(), BaseURL: srv.URL}
	_, _, err := f.Ensure(context.Background(), arch)
	require.ErrorContains(err, "checksum mismatch")
}

func TestArchFor(t *testing.T) {
	cases := map[string]Arch{"amd64": "amd64", "x86_64": "amd64", "arm64": "arm64", "aarch64": "arm64"}
	for in, want := range cases {
		got, err := ArchFor(in)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}
	_, err := ArchFor("riscv64")
	require.Error(t, err)
}
