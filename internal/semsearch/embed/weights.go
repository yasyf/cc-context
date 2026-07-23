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

	"github.com/yasyf/cc-context/internal/cache"
)

// ErrWeightsUnavailable marks the model weights as neither cached nor
// downloadable — a transport failure reaching HuggingFace with an empty cache.
// Tests skip on it; a corrupt cache or a wrong pin (checksum/HTTP-status
// mismatch) is a hard error instead, never this.
var ErrWeightsUnavailable = errors.New("model weights unavailable (no cache, no network)")

// weightFile is one model asset with its pinned lowercase-hex sha256, verified
// after every read and download so a corrupt or wrong-revision byte stream
// fails loud. Checksums were computed from the pinned revision on 2026-07-22.
type weightFile struct {
	name   string
	sha256 string
}

var weightFiles = [...]weightFile{
	{"config.json", "148e5691a6fcc553437156859701fba017a1ba5d340b170f17e0f3668fb861a7"},
	{"tokenizer.json", "107bbdcbad4bff1d299b7a4c3a2fb17c52890688b7dd0e4c9deab79d3c4f3d45"},
	{"model.safetensors", "75cf7a6c2171b230ad19b1e7d8e0b1aee86da5a02af8e7cacedd9921d227623c"},
}

// maxWeightFileBytes bounds each model file's download before its checksum is
// verified, so a broken or hostile mirror streaming an endless 200 cannot
// exhaust memory ahead of the integrity gate. The pinned model is ~30 MB total
// (config.json and tokenizer.json are tiny; model.safetensors is the bulk), so
// this 256 MiB ceiling is generous headroom while still capping the read.
// verifyChecksum stays the integrity gate; this is only a DoS guard. A var, not
// a const, so the download test can lower it.
var maxWeightFileBytes int64 = 256 << 20

// modelBlobs is the three-file model2vec payload the WASM engine loads.
type modelBlobs struct {
	tokenizer []byte
	model     []byte
	config    []byte
}

// resolveWeights returns the pinned model blobs, downloading any that are
// missing or checksum-stale into cache.Dir("semsearch", "models", Revision).
// The whole resolve runs under a cross-process lock so concurrent engines never
// race the same download.
func resolveWeights(ctx context.Context) (*modelBlobs, error) {
	dir, err := cache.Dir("semsearch", "models", Revision)
	if err != nil {
		return nil, fmt.Errorf("resolve model cache dir: %w", err)
	}
	blobs := &modelBlobs{}
	err = cache.WithLock(ctx, dir, "download", func() error {
		for i := range weightFiles {
			data, err := ensureFile(ctx, dir, weightFiles[i])
			if err != nil {
				return err
			}
			switch weightFiles[i].name {
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

// ensureFile returns the file's verified bytes, trusting a cache hit only when
// its checksum matches and otherwise downloading a fresh copy.
func ensureFile(ctx context.Context, dir string, wf weightFile) ([]byte, error) {
	path := filepath.Join(dir, wf.name)
	if data, err := os.ReadFile(path); err == nil && verifyChecksum(data, wf.sha256) == nil { //nolint:gosec // path is rooted at the cache dir
		return data, nil
	}
	data, err := download(ctx, wf)
	if err != nil {
		return nil, err
	}
	if err := verifyChecksum(data, wf.sha256); err != nil {
		return nil, fmt.Errorf("%s@%s: %w", wf.name, Revision, err)
	}
	if err := cache.Store(path, data, 0o644); err != nil {
		return nil, fmt.Errorf("cache %s: %w", wf.name, err)
	}
	return data, nil
}

// download fetches one file from the pinned revision. A transport failure (no
// network) becomes ErrWeightsUnavailable so callers can skip offline; a non-2xx
// status is a hard error, since it means the pin itself is wrong.
func download(ctx context.Context, wf weightFile) ([]byte, error) {
	url := fmt.Sprintf("%s/%s/resolve/%s/%s", endpoint(), Repo, Revision, wf.name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request for %s: %w", wf.name, err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: fetch %s: %w", ErrWeightsUnavailable, wf.name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("fetch %s: unexpected status %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxWeightFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read %s: %w", ErrWeightsUnavailable, wf.name, err)
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
