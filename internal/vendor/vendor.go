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

	"github.com/yasyf/cc-context/internal/cache"
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
	// BinInArchive is the archive entry to extract by name, required for a zip that
	// ships more than one entry (e.g. tilth's Windows zip).
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

	dir, err := cache.Dir("bin")
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, t.binaryName(asset))
	if executable(dst) {
		return dst, nil
	}

	lockCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	err = cache.WithLock(lockCtx, dir, t.Name, func() error {
		// Another holder of the lock may have provisioned it while we waited.
		if executable(dst) {
			return nil
		}
		return t.provision(ctx, asset, dst)
	})
	if err != nil {
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
	return cache.Store(dst, bin, 0o700)
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
// by name is required because a zip may ship more than one entry.
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

// executable reports whether path is a regular file with an owner-execute bit
// (or any file on Windows, where the bit is meaningless).
func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	return runtime.GOOS == "windows" || info.Mode().Perm()&0o100 != 0
}
