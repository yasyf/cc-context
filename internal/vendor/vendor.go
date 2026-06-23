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
	"strings"
	"time"
)

// downloadTimeout bounds a single provisioning attempt end to end.
const downloadTimeout = 5 * time.Minute

// platform names the OS/arch pair a release asset targets.
type platform struct {
	goos   string
	goarch string
}

// Tool describes a pinned, downloadable engine binary: where its per-platform
// release archives live, the committed sha256 of each, and which entry inside the
// archive is the binary to extract.
type Tool struct {
	// Name is the binary's lookup and extract name ("tilth", "ast-grep").
	Name string
	// Version is the pinned release tag.
	Version string
	// ReleaseBase is the release download root for Version.
	ReleaseBase string
	// Assets maps a platform to its literal release asset filename.
	Assets map[platform]string
	// Checksums pins the sha256 of each asset by filename. The pinned releases
	// publish no checksum manifest, so these committed digests are the integrity
	// control: a downloaded archive whose digest is absent here, or mismatches, is
	// refused. Bump alongside Version.
	Checksums map[string]string
	// BinInArchive is the archive entry to extract. Tilth's tar.gz archives ship a
	// single binary, but the ast-grep zip ships both "ast-grep" and "sg", so the
	// entry must be selected by name.
	BinInArchive string
}

// Ensure returns the path to the tool's pinned binary, downloading and caching it
// on first use. Repeat calls return the cached path without re-downloading.
// Concurrent callers coordinate through a per-tool advisory file lock so only one
// download runs at a time and different tools never serialize against each other.
func Ensure(ctx context.Context, t Tool) (string, error) {
	asset, err := t.assetFor(platform{runtime.GOOS, runtime.GOARCH})
	if err != nil {
		return "", err
	}

	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, t.binaryName(asset))
	if executable(dst) {
		return dst, nil
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("create cache dir %q: %w", dir, err)
	}

	lockCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	unlock, err := lock(lockCtx, dir, t.Name)
	if err != nil {
		return "", err
	}
	defer unlock()

	// Another holder of the lock may have provisioned it while we waited.
	if executable(dst) {
		return dst, nil
	}

	if err := t.provision(ctx, asset, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// assetFor maps an OS/arch pair to its release asset, erroring on unsupported
// platforms.
func (t Tool) assetFor(p platform) (string, error) {
	asset, ok := t.Assets[p]
	if !ok {
		return "", fmt.Errorf("%s %s: unsupported platform %s/%s", t.Name, t.Version, p.goos, p.goarch)
	}
	return asset, nil
}

// assetURL is the release download URL for a named asset of the pinned version.
func (t Tool) assetURL(asset string) string {
	return t.ReleaseBase + "/" + asset
}

// binaryName is the cached filename for the tool's extracted binary on this
// platform. The .exe suffix is carried over from a zip asset's Windows binary.
func (t Tool) binaryName(asset string) string {
	name := t.Name + "-" + t.Version
	if strings.HasSuffix(asset, ".zip") && runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// provision downloads, verifies, extracts, and atomically installs the binary.
func (t Tool) provision(ctx context.Context, asset, dst string) error {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	archive, err := download(ctx, t.assetURL(asset))
	if err != nil {
		return err
	}

	if err := t.verify(asset, archive); err != nil {
		return err
	}

	bin, err := t.extract(asset, archive)
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

// verify checks archive's sha256 against the digest pinned in t.Checksums. An
// asset with no pinned digest, or a digest mismatch, is a hard error: the binary
// is refused rather than installed unverified.
func (t Tool) verify(asset string, archive []byte) error {
	want, ok := t.Checksums[asset]
	if !ok {
		return fmt.Errorf("%s %s: no pinned checksum for %q; refusing to install unverified asset", t.Name, t.Version, asset)
	}
	got := sha256.Sum256(archive)
	if hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s %s: checksum mismatch for %q", t.Name, t.Version, asset)
	}
	return nil
}

// extract pulls the tool's binary out of the archive bytes, dispatching on the
// asset's archive type.
func (t Tool) extract(asset string, archive []byte) ([]byte, error) {
	if strings.HasSuffix(asset, ".zip") {
		return t.extractZip(archive)
	}
	return t.extractTarGz(archive)
}

// extractTarGz returns the first regular file from a gzip-compressed tarball.
func (t Tool) extractTarGz(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s: no binary in tarball", t.Name)
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

// extractZip returns the entry named t.BinInArchive from a zip archive. Selecting
// by name is required because the ast-grep zip ships both "ast-grep" and "sg".
func (t Tool) extractZip(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	for _, f := range zr.File {
		if f.Name != t.BinInArchive {
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
	return nil, fmt.Errorf("%s: entry %q not in zip", t.Name, t.BinInArchive)
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

// install writes bin to a sibling temp file and atomically renames it onto dst.
func install(bin []byte, dst string) error {
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".vendor-*")
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
	if err := os.Chmod(tmp.Name(), 0o700); err != nil { //nolint:gosec // the installed engine binary must be owner-executable
		return fmt.Errorf("chmod binary: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return fmt.Errorf("install binary %q: %w", dst, err)
	}
	return nil
}

// lock acquires an exclusive OS advisory lock on dir/.<name>.lock, retrying until
// it succeeds or ctx is done. The per-tool lock name keeps tilth and ast-grep
// from serializing against each other. The lock is an advisory file lock the
// kernel releases automatically on process death, so a SIGKILLed holder cannot
// leave a stale lock that hangs every later provisioning attempt. The returned
// func releases the lock and closes the fd; the lockfile itself is left in place
// (removing a flock'd file is racy).
func lock(ctx context.Context, dir, name string) (func(), error) {
	path := filepath.Join(dir, "."+name+".lock")
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
