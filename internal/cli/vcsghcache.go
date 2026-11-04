package cli

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/ghapi"
)

const (
	ghCacheMaxAnswer = 1 << 20
	ghCacheRetention = 24 * time.Hour
	ghEveryRepo      = "_all"
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

// ghStore stores a read's answer as of started, the moment before gh ran, so a
// write that lands while the read is in flight still expires it. It sweeps the
// entry's shard of answers past ghCacheRetention, which keeps the store bounded
// with no pass of its own.
func ghStore(key string, stdout []byte, started time.Time) error {
	path, err := ghCachePath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("vcs gh: create %s: %w", filepath.Dir(path), err)
	}
	data, err := json.Marshal(ghCacheEntry{At: started.UTC(), Stdout: stdout})
	if err != nil {
		return fmt.Errorf("vcs gh: encode cached answer: %w", err)
	}
	if err := cache.Store(path, data, 0o600); err != nil {
		return err
	}
	ghSweepShard(filepath.Dir(path), time.Now().Add(-ghCacheRetention))
	return nil
}

func ghSweepShard(dir string, before time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.ModTime().Before(before) {
			_ = os.Remove(filepath.Join(dir, entry.Name()))
		}
	}
}

func ghInvalidationPath(repo string) (string, error) {
	dir, err := ghapi.QuotaDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "written", strings.ReplaceAll(strings.ToLower(repo), "/", "__")), nil
}

// ghInvalidate expires every answer cached for repo, or for every repository
// when repo is ghEveryRepo. A failure here costs at most one stale read inside
// its max age, so it never fails the write that caused it.
func ghInvalidate(repo string) {
	path, err := ghInvalidationPath(repo)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	_ = os.WriteFile(path, nil, 0o600)
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

// ghInvalidatedAt returns the last write that expires an answer scoped to scope,
// or to dir's checkout when scope is "".
func ghInvalidatedAt(ctx context.Context, scope, dir string) time.Time {
	repo := scope
	if repo == "" {
		repo = ghCheckoutRepo(ctx, dir)
	}
	at := ghWrittenAt(ghEveryRepo)
	if repo == "" {
		return at
	}
	if written := ghWrittenAt(repo); written.After(at) {
		return written
	}
	return at
}

func ghWrittenAt(repo string) time.Time {
	path, err := ghInvalidationPath(repo)
	if err != nil {
		return time.Time{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// ghWrittenRepo returns the repository a gh write changes: the one it names,
// else ghEveryRepo for a GraphQL mutation, which names its target by node id,
// else the checkout's.
func ghWrittenRepo(ctx context.Context, argv []string, checkout func() string) string {
	if repo := cmp.Or(ghTargetRepo(ctx, argv), ghGraphQLRepo(argv)); repo != "" {
		return repo
	}
	if argv[0] == "api" {
		if _, endpoint := ghAPIRequest(argv[1:]); ghapi.Resource(endpoint) == ghapi.ResourceGraphQL {
			return ghEveryRepo
		}
	}
	return cmp.Or(checkout(), ghEveryRepo)
}

var (
	ghGraphQLLiteralRepo = regexp.MustCompile(`repository\(\s*owner:\s*"([^"]+)",?\s*name:\s*"([^"]+)"`)
	ghGraphQLOwnerField  = regexp.MustCompile(`^owner=([^@{][^/]*)$`)
	ghGraphQLNameField   = regexp.MustCompile(`^(?:name|repo)=([^@{][^/]*)$`)
)

// ghGraphQLRepo returns the OWNER/REPO a gh api graphql call names, as owner and
// name (or repo) fields or as literal repository arguments, "" when it names none.
func ghGraphQLRepo(argv []string) string {
	if len(argv) < 2 || argv[0] != "api" {
		return ""
	}
	if _, endpoint := ghAPIRequest(argv[1:]); ghapi.Resource(endpoint) != ghapi.ResourceGraphQL {
		return ""
	}
	var owner, name string
	for _, arg := range argv[1:] {
		if m := ghGraphQLLiteralRepo.FindStringSubmatch(arg); m != nil {
			return m[1] + "/" + m[2]
		}
		if m := ghGraphQLOwnerField.FindStringSubmatch(arg); m != nil {
			owner = m[1]
		}
		if m := ghGraphQLNameField.FindStringSubmatch(arg); m != nil {
			name = m[1]
		}
	}
	if owner == "" || name == "" {
		return ""
	}
	return owner + "/" + name
}
