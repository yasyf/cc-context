package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// prBodyStdin is the --pr-body-file value that reads the body from stdin.
const prBodyStdin = "-"

// prURLMarker separates a pull request URL's repository from its number.
const prURLMarker = "/pull/"

// prState is the open pull request ship found for a branch.
type prState struct {
	Number  int    `json:"number"`
	URL     string `json:"url"`
	IsDraft bool   `json:"isDraft"`
}

// prMeta is the caller's stated pull request metadata; an unset field is never
// written, so a re-ship that passes neither leaves a human's edits alone.
type prMeta struct {
	title    string
	bodyPath string // materialized: a real path, never "-"
	draft    *bool  // nil unless --draft/--publish was explicitly Changed
}

// stated names the fields this invocation restated, in report order. No stated
// field means no gh pr edit at all, which is what keeps a re-ship off a
// description someone hand-edited.
func (m prMeta) stated() []string {
	var fields []string
	if m.title != "" {
		fields = append(fields, "title")
	}
	if m.bodyPath != "" {
		fields = append(fields, "body")
	}
	return fields
}

// shipPRRequested reports whether this invocation asked ship to touch a pull
// request at all, so a ship that did not ask makes no gh pr call. A bare
// --draft/--publish only counts outside the graphite lane: gt submit already
// owns the draft state of every PR it opens.
func shipPRRequested(cmd *cobra.Command, l lane, o shipOpts) bool {
	switch {
	case o.noPR:
		return false
	case len(o.prTitle) > 0 || len(o.prBodyFile) > 0:
		return true
	default:
		return !l.gt && (cmd.Flags().Changed("draft") || cmd.Flags().Changed("publish"))
	}
}

// resolvePRMeta parses the repeatable pull request flags into one entry per
// branch, resolving a bare value against tip. It runs before any mutation and
// checks every body file it is handed, so an unreadable path refuses with the
// working copy untouched. The returned cleanup, always non-nil, removes the
// temp file a "-" occurrence materialized stdin into.
func resolvePRMeta(cmd *cobra.Command, o shipOpts, tip string) (map[string]prMeta, func(), error) {
	cleanup := func() {}
	meta := map[string]prMeta{}
	for _, value := range o.prTitle {
		branch, title := splitPRValue(value, tip)
		entry := meta[branch]
		if entry.title != "" {
			return nil, cleanup, fmt.Errorf("ship: --pr-title given twice for branch %s", branch)
		}
		entry.title = title
		meta[branch] = entry
	}

	stdinTaken := false
	for _, value := range o.prBodyFile {
		branch, path := splitPRValue(value, tip)
		entry := meta[branch]
		if entry.bodyPath != "" {
			return nil, cleanup, fmt.Errorf("ship: --pr-body-file given twice for branch %s", branch)
		}
		switch {
		case path != prBodyStdin:
			if _, err := os.Stat(path); err != nil {
				return nil, cleanup, fmt.Errorf("ship: --pr-body-file %s: %w", path, err)
			}
		case stdinTaken:
			return nil, cleanup, errors.New(`ship: only one --pr-body-file may read stdin ("-")`)
		case !stdinPiped(cmd):
			return nil, cleanup, errors.New(`ship: --pr-body-file - reads the body from stdin, which is a terminal — pipe the body in or pass a path`)
		default:
			tmp, remove, err := materializeStdin(cmd.InOrStdin())
			if err != nil {
				return nil, cleanup, err
			}
			path, cleanup, stdinTaken = tmp, remove, true
		}
		entry.bodyPath = path
		meta[branch] = entry
	}

	if cmd.Flags().Changed("draft") || cmd.Flags().Changed("publish") {
		entry := meta[tip]
		draft := o.draft
		entry.draft = &draft
		meta[tip] = entry
	}
	return meta, cleanup, nil
}

// splitPRValue splits a repeatable pull request flag value into the branch it
// scopes and the value itself. A value is branch-scoped only when the text
// before its first "=" is a name git would accept as a branch, so a bare title
// carrying an "=" still lands on tip.
func splitPRValue(value, tip string) (branch, rest string) {
	name, rest, ok := strings.Cut(value, "=")
	if !ok || strings.ContainsAny(name, " \t") || !legalBranchName(name) {
		return tip, value
	}
	return name, rest
}

// materializeStdin writes r to a temp file, because ship needs the body as a
// path: gh takes --body-file, ship may need the body twice in one run when a
// create races into an edit, and it has to be readable before the commit forms.
func materializeStdin(r io.Reader) (string, func(), error) {
	f, err := os.CreateTemp("", "ccx-pr-body-*")
	if err != nil {
		return "", nil, fmt.Errorf("ship: create pr body file: %w", err)
	}
	path := f.Name()
	remove := func() { _ = os.Remove(path) }
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		remove()
		return "", nil, fmt.Errorf("ship: read pr body from stdin: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", nil, fmt.Errorf("ship: write pr body file: %w", err)
	}
	return path, remove, nil
}

// shipPRRepo resolves the repository the pull request step names in --repo, and
// does it before any mutation: an unreachable GitHub is a refusal, not a
// half-finished ship. A plan that stays on trunk outside the graphite lane
// opens no pull request, so it needs no repository.
func shipPRRepo(ctx context.Context, l lane, plan branchPlan) (string, error) {
	if !l.gt && plan.trunk != "" && plan.name == plan.trunk {
		return "", nil
	}
	repo, err := vcs.LookupRepo(ctx, l.root, false)
	if err != nil {
		return "", fmt.Errorf("ship: a pull request needs GitHub metadata: %w", err)
	}
	return repo.NameWithOwner, nil
}

// shipPR is the single pull-request-owning step for every lane. It opens the
// branch's pull request when there is none and restates exactly the fields this
// invocation named when there is, so a description someone edited by hand
// survives a re-ship that does not mention it.
func shipPR(ctx context.Context, l lane, nwo, branch, trunk, subject string, meta map[string]prMeta) (string, error) {
	if l.gt {
		return shipPRGT(ctx, l, nwo, meta)
	}
	// A pull request from trunk into trunk is not a thing, and on a personal
	// repository that is the normal place to ship from.
	if trunk != "" && branch == trunk {
		return "no PR (on trunk)", nil
	}
	m := meta[branch]
	pr, found, err := lookupPR(ctx, l.root, nwo, branch)
	if err != nil {
		return "", err
	}
	if !found {
		return shipPRCreate(ctx, l.root, nwo, branch, trunk, subject, m)
	}
	return shipPREdit(ctx, l.root, nwo, pr, m)
}

// lookupPR resolves branch's open pull request through gh pr list, which exits 0
// with an empty array when there is none — unlike gh pr view, whose only signal
// is a stderr string reviews.go has to match on. --state open matters: a merged
// pull request on a reused branch name must never be edited.
func lookupPR(ctx context.Context, root, nwo, branch string) (prState, bool, error) {
	argv := []string{"pr", "list", "--repo", nwo, "--head", branch, "--state", "open", "--json", "number,url,isDraft", "--limit", "1"}
	out, err := render.RunCLIDir(ctx, root, "gh", argv)
	if err != nil {
		return prState{}, false, fmt.Errorf("ship: gh pr list: %w", err)
	}
	var prs []prState
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return prState{}, false, fmt.Errorf("ship: parse gh pr list: %w", err)
	}
	if len(prs) == 0 {
		return prState{}, false, nil
	}
	return prs[0], true, nil
}

// shipPRCreate opens the branch's pull request. --repo and an explicit --base
// are load-bearing: from a fork, non-interactive gh pr create otherwise resolves
// the base repository to the parent and can target upstream. It never passes
// --fill, which would publish withSessionTrailer's Claude-Session-Id line into
// the description.
func shipPRCreate(ctx context.Context, root, nwo, branch, trunk, subject string, m prMeta) (string, error) {
	title := m.title
	if title == "" {
		title = subject
	}
	argv := []string{"pr", "create", "--repo", nwo, "--head", branch, "--base", trunk, "--title", title}
	if m.bodyPath != "" {
		argv = append(argv, "--body-file", m.bodyPath)
	} else {
		argv = append(argv, "--body", "")
	}
	draft := m.draft != nil && *m.draft
	if draft {
		argv = append(argv, "--draft")
	}
	out, err := render.RunCLIDir(ctx, root, "gh", argv)
	if err != nil {
		return "", fmt.Errorf("ship: gh pr create: %w", err)
	}
	url := strings.TrimSpace(out)
	number, err := prNumberFromURL(url)
	if err != nil {
		return "", err
	}
	seg := fmt.Sprintf("opened PR #%d %s", number, url)
	if draft {
		seg += " [draft]"
	}
	return seg, nil
}

// shipPREdit restates the fields this invocation named on an existing pull
// request. Zero stated fields means zero calls.
func shipPREdit(ctx context.Context, root, nwo string, pr prState, m prMeta) (string, error) {
	fields := m.stated()
	if len(fields) > 0 {
		if _, err := render.RunCLIDir(ctx, root, "gh", prEditArgv(nwo, pr.Number, m)); err != nil {
			return "", fmt.Errorf("ship: gh pr edit: %w", err)
		}
	}
	// gh pr edit has no draft toggle, so a transition is its own verb — and only
	// a real transition is worth a call.
	if m.draft != nil && *m.draft != pr.IsDraft {
		argv := []string{"pr", "ready", strconv.Itoa(pr.Number), "--repo", nwo}
		field := "ready"
		if *m.draft {
			argv = append(argv, "--undo")
			field = "draft"
		}
		if _, err := render.RunCLIDir(ctx, root, "gh", argv); err != nil {
			return "", fmt.Errorf("ship: gh pr ready: %w", err)
		}
		fields = append(fields, field)
	}
	if len(fields) == 0 {
		return "", nil
	}
	return fmt.Sprintf("updated PR #%d %s (%s)", pr.Number, pr.URL, strings.Join(fields, ", ")), nil
}

func prEditArgv(nwo string, number int, m prMeta) []string {
	argv := []string{"pr", "edit", strconv.Itoa(number), "--repo", nwo}
	if m.title != "" {
		argv = append(argv, "--title", m.title)
	}
	if m.bodyPath != "" {
		argv = append(argv, "--body-file", m.bodyPath)
	}
	return argv
}

// shipPRGT backfills the pull requests gt submit just opened. The submit runs
// with --no-edit, so every PR it creates arrives bodyless, and restating the
// branches this invocation named is the only way a downstack PR gets a body at
// all. A branch the caller did not name has nothing to write, so it is left
// alone whether or not it already has one.
func shipPRGT(ctx context.Context, l lane, nwo string, meta map[string]prMeta) (string, error) {
	branch, err := gitCurrentBranch(ctx)
	if err != nil {
		return "", err
	}
	state, err := gtStateQuery(ctx)
	if err != nil {
		return "", err
	}
	trunk, err := gtTrunkBranch(state)
	if err != nil {
		return "", err
	}
	if branch == "" || branch == trunk {
		return "", nil
	}
	chain, err := gtDownstack(state, branch, trunk)
	if err != nil {
		return "", err
	}

	// infoDownstack orders the stack base-first; the report reads tip-first, the
	// way the submit segment ahead of it names the tip.
	entries := infoDownstack(ctx, l.root, chain)
	segs := make([]string, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		entry := entries[i]
		m := meta[entry.Branch]
		fields := m.stated()
		if len(fields) == 0 {
			continue
		}
		if entry.PR == 0 {
			return "", fmt.Errorf("ship: --pr-title/--pr-body-file named %s, which has no pull request", entry.Branch)
		}
		if _, err := render.RunCLIDir(ctx, l.root, "gh", prEditArgv(nwo, entry.PR, m)); err != nil {
			return "", fmt.Errorf("ship: gh pr edit: %w", err)
		}
		segs = append(segs, fmt.Sprintf("PR #%d %s", entry.PR, strings.Join(fields, "+")))
	}
	if len(segs) == 0 {
		return "", nil
	}
	return "set " + strings.Join(segs, ", "), nil
}

// prNumberFromURL reads the pull request number off the URL gh pr create prints,
// which is its only machine-readable output.
func prNumberFromURL(url string) (int, error) {
	i := strings.LastIndex(url, prURLMarker)
	if i < 0 {
		return 0, fmt.Errorf("ship: gh pr create printed no pull request URL: %q", url)
	}
	number, err := strconv.Atoi(strings.Trim(url[i+len(prURLMarker):], "/"))
	if err != nil {
		return 0, fmt.Errorf("ship: gh pr create printed a malformed pull request URL: %q", url)
	}
	return number, nil
}
