package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/ghapi"
)

type ghCacheEntry struct {
	At     time.Time `json:"at"`
	Repo   string    `json:"repo"`
	Argv   []string  `json:"argv"`
	Stdout []byte    `json:"stdout"`
}

func ghCacheKey(repo string, argv []string, stdin []byte) string {
	sum := sha256.Sum256([]byte(strings.ToLower(repo) + "\x00" + strings.Join(argv, "\x00") + "\x00\x00" + string(stdin)))
	return hex.EncodeToString(sum[:])
}

func ghCachePath(key string) (string, error) {
	dir, err := ghapi.QuotaDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "responses", key[:2], key+".json"), nil
}

func ghCached(key string) (ghCacheEntry, bool) {
	path, err := ghCachePath(key)
	if err != nil {
		return ghCacheEntry{}, false
	}
	data, err := os.ReadFile(path) //nolint:gosec // under the machine's own cache dir
	if err != nil {
		return ghCacheEntry{}, false
	}
	var entry ghCacheEntry
	if json.Unmarshal(data, &entry) != nil {
		return ghCacheEntry{}, false
	}
	return entry, true
}

func ghStore(key string, stdout []byte) error {
	path, err := ghCachePath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("vcs gh: create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.Marshal(ghCacheEntry{At: time.Now().UTC(), Stdout: stdout})
	if err != nil {
		return fmt.Errorf("vcs gh: encode cached answer: %w", err)
	}
	return cache.Store(path, data, 0o600)
}
