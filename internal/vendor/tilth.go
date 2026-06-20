// Package vendor provisions the pinned engine binaries.
package vendor

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// tilthVersion is the pinned tilth release tag.
const tilthVersion = "v0.9.0"

// releaseBase is the GitHub release download root for the pinned version.
const releaseBase = "https://github.com/jahala/tilth/releases/download/" + tilthVersion

// downloadTimeout bounds a single provisioning attempt end to end.
const downloadTimeout = 5 * time.Minute

// assetChecksums pins the sha256 of each v0.9.0 release asset, captured at vendor
// time. The pinned release publishes no checksum manifest, so these committed
// digests are the integrity control: a downloaded archive whose digest is absent
// here, or mismatches, is refused. Bump alongside tilthVersion.
var assetChecksums = map[string]string{
	"tilth-aarch64-apple-darwin.tar.gz":       "cdded363183c8b6ad276c8d049bc3b8b2dfa8c7e57d846c9bb4352f3515595fd",
	"tilth-x86_64-apple-darwin.tar.gz":        "635330817ac68cb3b7192f56f1cdbde27152331afb6369a4ade2f5349b167b2c",
	"tilth-aarch64-unknown-linux-musl.tar.gz": "2fa3ca73f089bdf037c7d5bbb951b5f8d7aa53de834753645a01b12a67cf67b6",
	"tilth-x86_64-unknown-linux-musl.tar.gz":  "6073bc83d3836913195be01bd953c6e0e6058d5774b216b84da71b87d6bf769c",
	"tilth-x86_64-pc-windows-msvc.zip":        "eb277008adf8a50dad3d374d028be6ff9472d9ef0ebc5118d1eb28bfa1c5be7d",
}

// platform names the OS/arch pair a tilth asset targets.
type platform struct {
	goos   string
	goarch string
}

// asset names the release artifact for a platform and whether the extracted
// binary carries a .exe suffix.
type asset struct {
	name    string
	windows bool
}

// EnsureTilth returns the path to the pinned tilth binary, downloading and
// caching it on first use. Repeat calls return the cached path without
// re-downloading. Concurrent callers coordinate through an advisory file lock so
// only one download runs at a time.
func EnsureTilth(ctx context.Context) (string, error) {
	a, err := assetFor(platform{runtime.GOOS, runtime.GOARCH})
	if err != nil {
		return "", err
	}

	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, binaryName(a))
	if executable(dst) {
		return dst, nil
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create cache dir %q: %w", dir, err)
	}

	lockCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	unlock, err := lock(lockCtx, dir)
	if err != nil {
		return "", err
	}
	defer unlock()

	// Another holder of the lock may have provisioned it while we waited.
	if executable(dst) {
		return dst, nil
	}

	if err := provision(ctx, a, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// assetFor maps an OS/arch pair to its release asset, erroring on unsupported
// platforms.
func assetFor(p platform) (asset, error) {
	table := map[platform]asset{
		{"darwin", "arm64"}:  {"tilth-aarch64-apple-darwin.tar.gz", false},
		{"darwin", "amd64"}:  {"tilth-x86_64-apple-darwin.tar.gz", false},
		{"linux", "arm64"}:   {"tilth-aarch64-unknown-linux-musl.tar.gz", false},
		{"linux", "amd64"}:   {"tilth-x86_64-unknown-linux-musl.tar.gz", false},
		{"windows", "amd64"}: {"tilth-x86_64-pc-windows-msvc.zip", true},
	}
	a, ok := table[p]
	if !ok {
		return asset{}, fmt.Errorf("tilth %s: unsupported platform %s/%s", tilthVersion, p.goos, p.goarch)
	}
	return a, nil
}

// assetURL is the release download URL for a named asset of the pinned version.
func assetURL(name string) string {
	return releaseBase + "/" + name
}

// binaryName is the cached filename for an asset's extracted binary.
func binaryName(a asset) string {
	if a.windows {
		return "tilth-" + tilthVersion + ".exe"
	}
	return "tilth-" + tilthVersion
}

// cacheDir resolves the directory holding cached binaries.
func cacheDir() (string, error) {
	if base := os.Getenv("CLAUDE_PLUGIN_DATA"); base != "" {
		return filepath.Join(base, "bin"), nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	return filepath.Join(base, "cc-context", "bin"), nil
}

// executable reports whether path is a regular file with an owner-execute bit
// (or any file on Windows, where the bit is meaningless).
func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o100 != 0
}

// provision downloads, verifies, extracts, and atomically installs the binary.
func provision(ctx context.Context, a asset, dst string) error {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	archive, err := download(ctx, assetURL(a.name))
	if err != nil {
		return err
	}

	if err := verify(a.name, archive); err != nil {
		return err
	}

	bin, err := extract(a, archive)
	if err != nil {
		return err
	}
	return install(bin, dst)
}

// download fetches url and returns its body.
func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request %q: %w", url, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %q: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %q: status %s", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body %q: %w", url, err)
	}
	return body, nil
}

// verify checks archive's sha256 against the digest pinned in assetChecksums. An
// asset with no pinned digest, or a digest mismatch, is a hard error: the binary
// is refused rather than installed unverified.
func verify(name string, archive []byte) error {
	want, ok := assetChecksums[name]
	if !ok {
		return fmt.Errorf("tilth %s: no pinned checksum for %q; refusing to install unverified asset", tilthVersion, name)
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("tilth %s: checksum mismatch for %q", tilthVersion, name)
	}
	return nil
}

// extract pulls the tilth binary out of the archive bytes.
func extract(a asset, archive []byte) ([]byte, error) {
	if a.windows {
		return extractZip(archive)
	}
	return extractTarGz(archive)
}

// extractTarGz returns the first regular file from a gzip-compressed tarball.
func extractTarGz(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("tilth: no binary in tarball")
		}
		if err != nil {
			return nil, fmt.Errorf("read tarball: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		bin, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("read tarball entry %q: %w", hdr.Name, err)
		}
		return bin, nil
	}
}

// extractZip returns the first non-directory entry from a zip archive.
func extractZip(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("open zip entry %q: %w", f.Name, err)
		}
		bin, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return nil, fmt.Errorf("read zip entry %q: %w", f.Name, err)
		}
		return bin, nil
	}
	return nil, errors.New("tilth: no binary in zip")
}

// install writes bin to a sibling temp file and atomically renames it onto dst.
func install(bin []byte, dst string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tilth-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	if _, err := tmp.Write(bin); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close binary: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o700); err != nil { //nolint:gosec // the installed tilth binary must be owner-executable
		return fmt.Errorf("chmod binary: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("install binary %q: %w", dst, err)
	}
	return nil
}

// lock acquires an exclusive OS advisory lock on dir/.tilth.lock, retrying until
// it succeeds or ctx is done. The lock is an advisory file lock the kernel
// releases automatically on process death, so a SIGKILLed holder cannot leave a
// stale lock that hangs every later provisioning attempt. The returned func
// releases the lock and closes the fd; the lockfile itself is left in place
// (removing a flock'd file is racy).
func lock(ctx context.Context, dir string) (func(), error) {
	path := filepath.Join(dir, ".tilth.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // lockfile path is under the trusted cache dir
	if err != nil {
		return nil, fmt.Errorf("open download lock %q: %w", path, err)
	}
	if err := flockExclusive(ctx, f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("acquire download lock %q: %w", path, err)
	}
	return func() {
		_ = flockUnlock(f)
		_ = f.Close()
	}, nil
}
