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
const githubSchema = 2

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
	Installation     bool      `json:"installation,omitempty"`
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
	return r.Installation || r.IsPrivate || r.Writable() || r.Affiliated
}

// AffiliationKnown reports whether Affiliated answers the membership question.
// A lookup asks GitHub about organization membership only for a public
// repository the viewer cannot administer, the one case where it decides Mine.
func (r Repo) AffiliationKnown() bool {
	return !r.IsPrivate && !r.Writable()
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
		if repo, ok := readRepo(path); ok && repo.Installation == installationToken(ctx) {
			return repo, nil
		}
	}
	if render.LookPath(ctx, "gh") == "" {
		return Repo{}, fmt.Errorf("%w: gh not on PATH", ErrNoGitHub)
	}

	var repo Repo
	err = cache.WithLock(ctx, filepath.Dir(path), "repo", func() error {
		if !refresh {
			if cached, ok := readRepo(path); ok && cached.Installation == installationToken(ctx) {
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
// their own lookup. It is exported so tests can seed or clear the
// record.
func RepoCachePath(ctx context.Context, root string) (string, error) {
	c, err := ResolveCheckout(root)
	if err != nil {
		return "", err
	}
	key := c.RepoKey()
	if installationToken(ctx) {
		key += "\x00installation"
	}
	sum := sha256.Sum256([]byte(key))
	dir, err := cache.Dir(ctx, "github", hex.EncodeToString(sum[:]))
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "repo.json"), nil
}

func installationToken(ctx context.Context) bool {
	token := render.Getenv(ctx, "GH_TOKEN")
	if token == "" {
		token = render.Getenv(ctx, "GITHUB_TOKEN")
	}
	return strings.HasPrefix(token, "ghs_")
}

func fetchRepo(ctx context.Context, root render.Dir) (Repo, error) {
	out, err := render.RunCLI(ctx, root, "gh", []string{"api", "repos/{owner}/{repo}"})
	if err != nil {
		return Repo{}, fmt.Errorf("%w: gh api repos/{owner}/{repo}: %w", ErrNoGitHub, err)
	}
	var payload struct {
		FullName string `json:"full_name"`
		Owner    struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"owner"`
		Private     bool            `json:"private"`
		Permissions restPermissions `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		return Repo{}, fmt.Errorf("parse gh api repos/{owner}/{repo}: %w", err)
	}

	repo := Repo{
		NameWithOwner:    payload.FullName,
		Owner:            payload.Owner.Login,
		IsPrivate:        payload.Private,
		ViewerPermission: payload.Permissions.role(),
		FetchedAt:        time.Now(),
	}
	if installationToken(ctx) {
		repo.Installation = true
		return repo, nil
	}
	var v viewer
	if repo.AffiliationKnown() && payload.Owner.Type == "Organization" {
		v, err = fetchViewer(ctx, repo.Owner)
	} else {
		v.Login, err = fetchLogin(ctx)
	}
	if err != nil {
		return Repo{}, err
	}
	repo.ViewerLogin = v.Login
	repo.Affiliated = affiliated(repo.Owner, v)
	return repo, nil
}

// restPermissions is the viewer's access as the REST repository payload
// spells it, one flag per role GitHub grants.
type restPermissions struct {
	Admin    bool `json:"admin"`
	Maintain bool `json:"maintain"`
	Push     bool `json:"push"`
	Triage   bool `json:"triage"`
	Pull     bool `json:"pull"`
}

// role names the highest role the flags grant in GraphQL's
// RepositoryPermission spelling, the one Repo.ViewerPermission carries.
func (p restPermissions) role() string {
	switch {
	case p.Admin:
		return "ADMIN"
	case p.Maintain:
		return "MAINTAIN"
	case p.Push:
		return "WRITE"
	case p.Triage:
		return "TRIAGE"
	case p.Pull:
		return "READ"
	default:
		return ""
	}
}

func fetchLogin(ctx context.Context) (string, error) {
	out, err := render.RunCLI(ctx, render.Ambient, "gh", []string{"api", "user"})
	if err != nil {
		return "", fmt.Errorf("%w: gh api user: %w", ErrNoGitHub, err)
	}
	var user struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal([]byte(out), &user); err != nil {
		return "", fmt.Errorf("parse gh api user: %w", err)
	}
	return user.Login, nil
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
