package overview

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// gitRunner is the git subprocess boundary for the ref and oid queries whose output
// carries no path, so no NUL framing applies; git is a package-level var so tests
// inject canned transcripts instead of shelling out. Every path-bearing query goes
// through internal/vcs instead, which owns the framing.
type gitRunner func(ctx context.Context, dir render.Dir, args ...string) (string, error)

var git gitRunner = runGit

func runGit(ctx context.Context, dir render.Dir, args ...string) (string, error) {
	return render.RunCLI(ctx, dir, "git", args)
}

// gitAnswers reports whether git can read a repository at root, so a non-colocated
// jj workspace (.jj, no .git) omits the git-backed sections instead of emitting "".
func gitAnswers(ctx context.Context, root render.Dir) bool {
	_, err := git(ctx, root, "rev-parse", "--git-dir")
	return err == nil
}

// gitLines renders the git-backed overview lines for the repo at root, in order: the
// state headline, then the churn line. A repository with no commits answers neither
// probe, so it yields no lines at all; every other probe failure comes back as an
// error rather than a silently missing line or segment.
func gitLines(ctx context.Context, root render.Dir) ([]string, error) {
	section, err := gitSection(ctx, root)
	if err != nil {
		return nil, err
	}
	if section == "" {
		return nil, nil
	}
	hot, err := hotLine(ctx, root)
	if err != nil {
		return nil, err
	}
	return []string{section, hot}, nil
}

// headHasCommit reports whether root's HEAD names a commit, the state a repo with
// no commits yet is in. `git rev-parse --verify --quiet HEAD` is the one form of
// the question with a real tri-state — 1 and silent for a branch with no commits,
// 0 for one with, 128 for a repository that cannot answer at all — so no failure
// has to be read as an absence.
func headHasCommit(ctx context.Context, root render.Dir) (bool, error) {
	_, code, stderr, err := render.RunCLIExitCode(ctx, root, "git", []string{"rev-parse", "--verify", "--quiet", "HEAD"})
	if err != nil {
		return false, fmt.Errorf("rev-parse HEAD: %w", err)
	}
	switch code {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("rev-parse HEAD: exit %d: %s", code, strings.TrimSpace(stderr))
	}
}

// gitSection renders "git: main @ a1b2c3d "release: v0.22.0" · 3 dirty · 1240 commits"
// for the repo at root. A detached HEAD drops the branch name. It returns "" when the
// repo has no commits, which headHasCommit establishes on its own so that every probe
// after it answers whenever it did — a failure there is returned rather than costing a
// segment or, worse, reading as the commitless repo.
func gitSection(ctx context.Context, root render.Dir) (string, error) {
	hasCommit, err := headHasCommit(ctx, root)
	if err != nil {
		return "", err
	}
	if !hasCommit {
		return "", nil
	}
	logOut, err := git(ctx, root, "log", "-1", "--format=%h%x00%s")
	if err != nil {
		return "", err
	}
	hash, subject, _ := strings.Cut(strings.TrimRight(logOut, "\n"), "\x00")

	var b strings.Builder
	b.WriteString("git: ")
	branch, err := git(ctx, root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if name := strings.TrimSpace(branch); name != "HEAD" {
		b.WriteString(name + " ")
	}
	b.WriteString("@ " + hash + ` "` + subject + `"`)

	dirty, err := vcs.GitStatus(ctx, vcs.GitArgs{Dir: root, Sub: []string{"status"}})
	if err != nil {
		return "", err
	}
	if len(dirty) > 0 {
		b.WriteString(" · " + strconv.Itoa(len(dirty)) + " dirty")
	}

	commits, err := git(ctx, root, "rev-list", "--count", "HEAD")
	if err != nil {
		return "", err
	}
	b.WriteString(" · " + strings.TrimSpace(commits) + " commits")
	return b.String(), nil
}

// hotDirLimit caps how many hot directories the churn section lists.
const hotDirLimit = 5

// hotLine renders "hot (90d): internal/cli (34), internal/web (21)" by aggregating the
// files changed in the last 90 days to their leading two path segments, top by count.
// It returns "" when no files changed.
func hotLine(ctx context.Context, root render.Dir) (string, error) {
	changed, err := vcs.GitPaths(ctx, vcs.GitArgs{
		Dir: root,
		Sub: []string{"log", "--since=90.days", "--name-only", "--format="},
	})
	if err != nil {
		return "", err
	}
	counts := map[string]int{}
	for _, p := range changed {
		if key := hotKey(p); key != "" {
			counts[key]++
		}
	}
	if len(counts) == 0 {
		return "", nil
	}
	type kv struct {
		dir string
		n   int
	}
	xs := make([]kv, 0, len(counts))
	for d, n := range counts {
		xs = append(xs, kv{d, n})
	}
	sort.Slice(xs, func(i, j int) bool {
		if xs[i].n != xs[j].n {
			return xs[i].n > xs[j].n
		}
		return xs[i].dir < xs[j].dir
	})
	if len(xs) > hotDirLimit {
		xs = xs[:hotDirLimit]
	}
	parts := make([]string, len(xs))
	for i, x := range xs {
		parts[i] = x.dir + " (" + strconv.Itoa(x.n) + ")"
	}
	return "hot (90d): " + strings.Join(parts, ", "), nil
}

// hotKey reduces a changed file path to its containing directory's leading two
// segments (internal/cli/foo.go → internal/cli), or "" for a root-level file.
func hotKey(p string) string {
	dir := path.Dir(path.Clean(p))
	if dir == "." || dir == "/" || dir == "" {
		return ""
	}
	segs := strings.Split(dir, "/")
	if len(segs) >= 2 {
		return segs[0] + "/" + segs[1]
	}
	return segs[0]
}
