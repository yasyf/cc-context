package embed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
)

// ErrWeightsUnavailable marks the model weights as neither cached nor
// downloadable — a transport failure reaching HuggingFace with an empty cache.
// Tests skip on it; a corrupt cache or a wrong pin (checksum/HTTP-status
// mismatch) is a hard error instead, never this.
var ErrWeightsUnavailable = errors.New("model weights unavailable (no cache, no network)")

// maxWeightFileBytes bounds each model file's download before its checksum is
// verified, so a broken or hostile mirror streaming an endless 200 cannot
// exhaust memory ahead of the integrity gate. The pinned model is ~30 MB total
// (config.json and tokenizer.json are tiny; model.safetensors is the bulk), so
// this 256 MiB ceiling is generous headroom while still capping the read.
// verifyChecksum stays the integrity gate; this is only a DoS guard. A var, not
// a const, so the download test can lower it.
var maxWeightFileBytes int64 = 256 << 20

// downloadTimeout bounds one weight file's fetch so a stalled mirror cannot
// wedge engine construction; generous for the ~30 MB model — mirrors the
// deleted uv driver's first-run bound. A var, not a const, so the download test
// can shorten it.
var downloadTimeout = 5 * time.Minute

// modelBlobs is the three-file model2vec payload the WASM engine loads.
type modelBlobs struct {
	tokenizer []byte
	model     []byte
	config    []byte
}

// resolveWeights returns the pinned model blobs, downloading any that are
// missing or checksum-stale into cache.Dir("semsearch", "models", <repo>,
// pin.Revision) — namespaced by repo and revision so multiple model pins never
// collide. The whole resolve runs under a cross-process lock so concurrent
// engines never race the same download.
func resolveWeights(ctx context.Context, pin ModelPin) (*modelBlobs, error) {
	dir, err := cache.Dir("semsearch", "models", sanitizeRepo(pin.Repo), pin.Revision)
	if err != nil {
		return nil, fmt.Errorf("resolve model cache dir: %w", err)
	}
	blobs := &modelBlobs{}
	err = cache.WithLock(ctx, dir, "download", func() error {
		for i := range pin.Files {
			data, err := ensureFile(ctx, dir, pin, pin.Files[i])
			if err != nil {
				return err
			}
			switch pin.Files[i].Name {
			case "config.json":
				blobs.config = data
			case "tokenizer.json":
				blobs.tokenizer = data
			case "model.safetensors":
				blobs.model = data
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return blobs, nil
}

// sanitizeRepo turns a HuggingFace repo id ("owner/name") into a single
// filesystem-safe path segment by replacing its slash, so the weights cache
// namespaces by repo without nesting an owner directory.
func sanitizeRepo(repo string) string {
	return strings.ReplaceAll(repo, "/", "_")
}

// ensureFile returns the file's verified bytes, trusting a cache hit only when
// its checksum matches and otherwise downloading a fresh copy from the pin.
func ensureFile(ctx context.Context, dir string, pin ModelPin, wf WeightFile) ([]byte, error) {
	path := filepath.Join(dir, wf.Name)
	if data, err := os.ReadFile(path); err == nil && verifyChecksum(data, wf.SHA256) == nil { //nolint:gosec // path is rooted at the cache dir
		return data, nil
	}
	data, err := download(ctx, pin, wf)
	if err != nil {
		return nil, err
	}
	if err := verifyChecksum(data, wf.SHA256); err != nil {
		return nil, fmt.Errorf("%s@%s: %w", wf.Name, pin.Revision, err)
	}
	if err := cache.Store(path, data, 0o644); err != nil {
		return nil, fmt.Errorf("cache %s: %w", wf.Name, err)
	}
	return data, nil
}

// download fetches one file from the pin's revision. A transport failure (no
// network) becomes ErrWeightsUnavailable so callers can skip offline; a non-2xx
// status is a hard error, since it means the pin itself is wrong.
func download(ctx context.Context, pin ModelPin, wf WeightFile) ([]byte, error) {
	url := fmt.Sprintf("%s/%s/resolve/%s/%s", endpoint(), pin.Repo, pin.Revision, wf.Name)
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", wf.Name, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: fetch %s: %w", ErrWeightsUnavailable, wf.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("fetch %s: unexpected status %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxWeightFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrWeightsUnavailable, wf.Name, err)
	}
	if int64(len(data)) > maxWeightFileBytes {
		return nil, fmt.Errorf("fetch %s: response exceeds the %d-byte cap (broken or hostile mirror)", url, maxWeightFileBytes)
	}
	return data, nil
}

// verifyChecksum reports whether data hashes to the pinned sha256.
func verifyChecksum(data []byte, want string) error {
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("checksum mismatch: got %s, want %s", got, want)
	}
	return nil
}

// endpoint honors the HuggingFace HF_ENDPOINT override (mirror hosts) and falls
// back to the public hub.
func endpoint() string {
	if e := os.Getenv("HF_ENDPOINT"); e != "" {
		return e
	}
	return "https://huggingface.co"
}
