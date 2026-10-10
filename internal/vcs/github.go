package vcs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/render"
)

// ErrNoGitHub wraps every reason a lookup cannot be made — gh off PATH, not
// signed in, no GitHub remote. Callers treat it as "unknown", never as "not
// yours": the lane gate only ever demotes on a positive answer.
var ErrNoGitHub = errors.New("github metadata unavailable")

// githubSchema is the on-disk format version of the cached repo record; a
// mismatch reads as a miss.
const githubSchema = 1

// githubTTL bounds how long a cached record is served before a refetch.
const githubTTL = 24 * time.Hour

// viewerQuery asks for the signed-in account and whether it belongs to the
// organization named by $owner, the two inputs to Repo.Affiliated. A token
// holding only the repo scope can read both; listing the viewer's organizations
// needs read:org.
const viewerQuery = "query=query($owner:String!){viewer{login} repositoryOwner(login:$owner){... on Organization{viewerIsAMember}}}"

// Repo is a repository's GitHub metadata: who owns it, how visible it is, and
// what the signed-in viewer may do with it.
type Repo struct {
	NameWithOwner    string    `json:"name_with_owner"`
	Owner            string    `json:"owner"`
	IsPrivate        bool      `json:"is_private"`
	ViewerLogin      string    `json:"viewer_login"`
	ViewerPermission string    `json:"viewer_permission"`
	Affiliated       bool      `json:"affiliated"`
	FetchedAt        time.Time `json:"fetched_at"`
}

// viewer is the signed-in GitHub account and whether it is a member of the
// organization that owns the repository being looked up.
type viewer struct {
	Login  string
	Member bool
}

type repoRecord struct {
	Schema int  `json:"schema"`
	Repo   Repo `json:"repo"`
}

// Writable reports whether the viewer's role can administer the repository:
// GitHub's ADMIN or MAINTAIN. A WRITE collaborator on someone else's public
// repo is deliberately excluded — pushing a branch is not owning the workflow.
func (r Repo) Writable() bool {
	switch r.ViewerPermission {
	case "ADMIN", "MAINTAIN":
		return true
	default:
		return false
	}
}

// Mine reports whether the repository is the viewer's own to run a stacking
// workflow on: any private repo they can read, one they administer or maintain,
// or one owned by them or an organization they belong to.
func (r Repo) Mine() bool {
	return r.IsPrivate || r.Writable() || r.Affiliated
}

// Personal reports whether the repository is owned by the viewer's own account
// rather than an organization — the case where committing straight to trunk is
// the established workflow.
func (r Repo) Personal() bool {
	return r.Owner != "" && strings.EqualFold(r.Owner, r.ViewerLogin)
}

// LookupRepo reads root's GitHub metadata, served from a 24h cache unless
// refresh forces a refetch. Every reason the answer is unknowable wraps
// ErrNoGitHub.
func LookupRepo(ctx context.Context, root render.Dir, refresh bool) (Repo, error) {
	path, err := RepoCachePath(ctx, string(root))
	if err != nil {
		return Repo{}, err
	}
	if !refresh {
		if repo, ok := readRepo(path); ok {
			return repo, nil
		}
	}
	if render.LookPath(ctx, "gh") == "" {
		return Repo{}, fmt.Errorf("%w: gh not on PATH", ErrNoGitHub)
	}

	var repo Repo
	err = cache.WithLock(ctx, filepath.Dir(path), "repo", func() error {
		if !refresh {
			if cached, ok := readRepo(path); ok {
				repo = cached
				return nil
			}
		}
		fetched, err := fetchRepo(ctx, root)
		if err != nil {
			return err
		}
		repo = fetched
		return storeRecord(path, repoRecord{Schema: githubSchema, Repo: repo})
	})
	if err != nil {
		return Repo{}, err
	}
	return repo, nil
}

// RepoCachePath resolves the cached-record path for the GitHub metadata of the
// repository root belongs to. The key is the repository, not the checkout, so
// a repository's linked worktrees share one record rather than each paying
// their own `gh repo view`. It is exported so tests can seed or clear the
// record.
func RepoCachePath(ctx context.Context, root string) (string, error) {
	c, err := ResolveCheckout(root)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(c.RepoKey()))
	dir, err := cache.Dir(ctx, "github", hex.EncodeToString(sum[:]))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "repo.json"), nil
}

func fetchRepo(ctx context.Context, root render.Dir) (Repo, error) {
	out, err := render.RunCLI(ctx, root, "gh", []string{"repo", "view", "--json", "nameWithOwner,owner,isPrivate,viewerPermission"})
	if err != nil {
		return Repo{}, fmt.Errorf("%w: gh repo view: %w", ErrNoGitHub, err)
	}
	var view struct {
		NameWithOwner string `json:"nameWithOwner"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
		IsPrivate        bool   `json:"isPrivate"`
		ViewerPermission string `json:"viewerPermission"`
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		return Repo{}, fmt.Errorf("parse gh repo view: %w", err)
	}

	v, err := fetchViewer(ctx, view.Owner.Login)
	if err != nil {
		return Repo{}, err
	}
	return Repo{
		NameWithOwner:    view.NameWithOwner,
		Owner:            view.Owner.Login,
		IsPrivate:        view.IsPrivate,
		ViewerLogin:      v.Login,
		ViewerPermission: view.ViewerPermission,
		Affiliated:       affiliated(view.Owner.Login, v),
		FetchedAt:        time.Now(),
	}, nil
}

func fetchViewer(ctx context.Context, owner string) (viewer, error) {
	out, err := render.RunCLI(ctx, render.Ambient, "gh", []string{"api", "graphql", "-f", viewerQuery, "-f", "owner=" + owner})
	if err != nil {
		return viewer{}, fmt.Errorf("%w: gh api graphql: %w", ErrNoGitHub, err)
	}
	var resp struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
			RepositoryOwner struct {
				ViewerIsAMember bool `json:"viewerIsAMember"`
			} `json:"repositoryOwner"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return viewer{}, fmt.Errorf("parse gh api graphql: %w", err)
	}
	return viewer{Login: resp.Data.Viewer.Login, Member: resp.Data.RepositoryOwner.ViewerIsAMember}, nil
}

// affiliated reports whether owner is the viewer itself or an organization it
// belongs to. GitHub logins are case-insensitive.
func affiliated(owner string, v viewer) bool {
	return v.Member || strings.EqualFold(owner, v.Login)
}

func readRepo(path string) (Repo, bool) {
	var rec repoRecord
	if !readRecord(path, &rec) || rec.Schema != githubSchema || time.Since(rec.Repo.FetchedAt) >= githubTTL {
		return Repo{}, false
	}
	return rec.Repo, true
}

// readRecord decodes path into out, reporting false for an absent or
// undecodable record — a cache miss, never an error.
func readRecord(path string, out any) bool {
	data, err := os.ReadFile(path) //nolint:gosec // path is rooted at the cache dir and keyed by sha256 hex
	if err != nil {
		return false
	}
	return json.Unmarshal(data, out) == nil
}

// storeRecord installs rec at path with owner-only perms: the record carries
// the viewer's login.
func storeRecord(path string, rec any) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal record for %q: %w", path, err)
	}
	return cache.Store(path, data, 0o600)
}
