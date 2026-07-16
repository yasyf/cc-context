package overview

import (
	"context"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
)

// gitRunner is the git subprocess boundary; git is a package-level var so tests inject
// canned transcripts instead of shelling out.
type gitRunner func(ctx context.Context, dir string, args ...string) (string, error)

var git gitRunner = runGit

func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	return render.RunCLI(ctx, "git", append([]string{"-C", dir}, args...))
}

// gitAnswers reports whether git can read a repository at root, so a non-colocated
// jj workspace (.jj, no .git) omits the git-backed sections instead of emitting "".
func gitAnswers(ctx context.Context, root string) bool {
	_, err := git(ctx, root, "rev-parse", "--git-dir")
	return err == nil
}

// gitSection renders "git: main @ a1b2c3d "release: v0.22.0" · 3 dirty · 1240 commits"
// for the repo at root. A detached HEAD drops the branch name. It returns "" when the
// repo has no commits (the log probe fails); the caller gates on VCS presence.
func gitSection(ctx context.Context, root string) string {
	logOut, err := git(ctx, root, "log", "-1", "--format=%h%x00%s")
	if err != nil {
		return ""
	}
	hash, subject, _ := strings.Cut(strings.TrimRight(logOut, "\n"), "\x00")
	if hash == "" {
		return ""
	}

	var b strings.Builder
	b.WriteString("git: ")
	if branch, err := git(ctx, root, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
		if name := strings.TrimSpace(branch); name != "" && name != "HEAD" {
			b.WriteString(name + " ")
		}
	}
	b.WriteString("@ " + hash + ` "` + subject + `"`)

	if out, err := git(ctx, root, "status", "--porcelain", "-z"); err == nil {
		if n := countPorcelain(out); n > 0 {
			b.WriteString(" · " + strconv.Itoa(n) + " dirty")
		}
	}
	if out, err := git(ctx, root, "rev-list", "--count", "HEAD"); err == nil {
		if n := strings.TrimSpace(out); n != "" {
			b.WriteString(" · " + n + " commits")
		}
	}
	return b.String()
}

// countPorcelain counts changed-file entries in `git status --porcelain -z` output,
// skipping the rename/copy origin-path field that trails an R/C entry.
func countPorcelain(out string) int {
	tokens := strings.Split(out, "\x00")
	n := 0
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		if len(t) < 3 || t[2] != ' ' {
			continue
		}
		n++
		if t[0] == 'R' || t[0] == 'C' || t[1] == 'R' || t[1] == 'C' {
			i++ // the next token is the origin path, not a new entry
		}
	}
	return n
}

// hotDirLimit caps how many hot directories the churn section lists.
const hotDirLimit = 5

// hotLine renders "hot (90d): internal/cli (34), internal/web (21)" by aggregating the
// files changed in the last 90 days to their leading two path segments, top by count.
// It returns "" when the log probe fails or no files changed.
func hotLine(ctx context.Context, root string) string {
	out, err := git(ctx, root, "log", "--since=90.days", "--name-only", "--format=")
	if err != nil {
		return ""
	}
	counts := map[string]int{}
	for _, ln := range strings.Split(out, "\n") {
		p := strings.TrimSpace(ln)
		if p == "" {
			continue
		}
		if key := hotKey(p); key != "" {
			counts[key]++
		}
	}
	if len(counts) == 0 {
		return ""
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
	return "hot (90d): " + strings.Join(parts, ", ")
}

// hotKey reduces a changed file path to its containing directory's leading two
// segments (internal/cli/foo.go → internal/cli), or "" for a root-level file.
func hotKey(p string) string {
	dir := path.Dir(path.Clean(strings.TrimPrefix(p, "./")))
	if dir == "." || dir == "/" || dir == "" {
		return ""
	}
	segs := strings.Split(dir, "/")
	if len(segs) >= 2 {
		return segs[0] + "/" + segs[1]
	}
	return segs[0]
}
