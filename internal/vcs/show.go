package vcs

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Commit is a single commit's header metadata plus the git commit range whose
// diff is exactly that commit's own change (parent..commit).
type Commit struct {
	ShortID string
	Author  string
	Email   string
	Date    string
	Subject string
	Body    string
	// Range is the parent..commit git range the diff pipeline consumes as its
	// source to render exactly this commit's change.
	Range string
}

// gitShowFormat lays the header fields out as NUL-separated records: short id,
// author name, author email, date, full id, parent ids, raw message.
const gitShowFormat = "%h%x00%an%x00%ae%x00%ad%x00%H%x00%P%x00%B"

// jjShowTemplate emits the same NUL-separated field layout as gitShowFormat so a
// single parser serves both VCSes.
const jjShowTemplate = `commit_id.short() ++ "\x00" ++ author.name() ++ "\x00" ++ author.email() ++ "\x00" ++ author.timestamp().format("%Y-%m-%d") ++ "\x00" ++ commit_id ++ "\x00" ++ parents.map(|c| c.commit_id()).join(" ") ++ "\x00" ++ description`

// Show resolves ref to its header metadata and single-commit diff range,
// VCS-aware. An empty ref selects the last commit (jj: @-, git: HEAD).
func Show(ctx context.Context, dir, ref string) (Commit, error) {
	if Detect(dir) == JJ {
		return showJJ(ctx, dir, ref)
	}
	return showGit(ctx, dir, ref)
}

func showGit(ctx context.Context, dir, ref string) (Commit, error) {
	if ref == "" {
		ref = "HEAD"
	}
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "show", "--no-patch", "--format="+gitShowFormat, "--date=short", ref).Output() //nolint:gosec // fixed git argv; only the working dir and ref vary
	if err != nil {
		return Commit{}, fmt.Errorf("git show %q: %w", ref, err)
	}
	return parseCommit(string(out))
}

func showJJ(ctx context.Context, dir, ref string) (Commit, error) {
	if ref == "" {
		ref = "@-"
	}
	rev := jjShowRevset(ctx, dir, ref, gitCommitID)
	cmd := exec.CommandContext(ctx, "jj", "log", "--no-graph", "-r", rev, "-T", jjShowTemplate) //nolint:gosec // fixed jj argv; only the working dir and revset vary
	// run inside the working copy so relative revsets like @- resolve
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return Commit{}, fmt.Errorf("jj log -r %q: %w", rev, err)
	}
	return parseCommit(string(out))
}

// gitRefResolver resolves a git symbolic ref to its commit id within dir,
// reporting ok=false when git cannot name it. It is injectable so the show-ref
// translation is table-testable without a live repo.
type gitRefResolver func(ctx context.Context, dir, ref string) (id string, ok bool)

// jjShowRevset translates a git symbolic ref (HEAD, HEAD~N, HEAD^, a branch, a
// tag, a sha) into the git commit id jj resolves it to, because jj cannot name
// git's symbolic refs. jj-native revsets (@, @-, set operators) short-circuit
// untranslated, and a ref git cannot resolve (a jj change id) passes through for
// jj to interpret.
func jjShowRevset(ctx context.Context, dir, ref string, resolve gitRefResolver) string {
	if isJJNativeRevset(ref) {
		return ref
	}
	if id, ok := resolve(ctx, dir, ref); ok {
		return id
	}
	return ref
}

// gitCommitID resolves ref to its commit id via git rev-parse, peeling annotated
// tags to the commit they point at. ok is false when git cannot resolve ref.
func gitCommitID(ctx context.Context, dir, ref string) (string, bool) {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Output() //nolint:gosec // fixed git argv; only the working dir and ref vary
	if err != nil {
		return "", false
	}
	id := strings.TrimSpace(string(out))
	if id == "" {
		return "", false
	}
	return id, true
}

// parseCommit decodes the shared NUL-separated header record into a Commit,
// splitting the raw message into subject (first line) and body (the rest). It
// fails loudly on a short record or a parentless (root) commit, which has no
// parent..commit range to diff.
func parseCommit(raw string) (Commit, error) {
	const fieldCount = 7
	parts := strings.SplitN(raw, "\x00", fieldCount)
	if len(parts) != fieldCount {
		return Commit{}, fmt.Errorf("parse commit: got %d fields, want %d", len(parts), fieldCount)
	}
	parents := strings.Fields(parts[5])
	if len(parents) == 0 {
		return Commit{}, fmt.Errorf("commit %s has no parent to diff against", parts[4])
	}
	subject, body, _ := strings.Cut(strings.TrimRight(parts[6], "\n"), "\n")
	return Commit{
		ShortID: parts[0],
		Author:  parts[1],
		Email:   parts[2],
		Date:    parts[3],
		Subject: subject,
		Body:    strings.TrimSpace(body),
		Range:   parents[0] + ".." + parts[4],
	}, nil
}
