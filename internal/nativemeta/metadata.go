// Package nativemeta maintains GitHub native stack catalogs and publication receipts.
package nativemeta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/render"
)

// StateFrozen marks branches that stack replay and publication must leave untouched.
const StateFrozen = "frozen"

// Ref captures the parent revision a branch was last based on.
type Ref struct {
	Ref string
	SHA string
}

// BranchState combines live Git identity with recorded replay and hold information.
type BranchState struct {
	Trunk        bool
	NeedsRestack bool
	Head         string
	State        string
	Parents      []Ref
}

// State indexes tracked branches and their trunks by branch name.
type State map[string]BranchState

// Row identifies stale or unresolved catalog entries for pruning.
type Row struct {
	Branch   string
	Parent   string
	Stale    bool
	Diverged bool
}

// Orphan retains a live child whose recorded parent ref disappeared.
type Orphan struct {
	Branch         string
	Parent         string
	ParentRevision string
}

// Version records the head and base observed at a successful publication.
type Version struct {
	HeadSha  string `json:"headSha"`
	BaseSha  string `json:"baseSha"`
	BaseName string `json:"baseName"`
}

// PublishedChildState exposes the recorded identity needed to compare a child with a publication receipt.
type PublishedChildState struct {
	Parent             string
	ParentRevision     string
	BranchRevision     string
	Validation         string
	State              string
	Children           string
	Submitted          string
	ParentHeadRevision string
}

type pullRequestRef struct {
	Number     int    `json:"number"`
	ID         string `json:"id,omitempty"`
	URL        string `json:"url,omitempty"`
	Merged     bool   `json:"merged,omitempty"`
	MergedHead string `json:"mergedHead,omitempty"`
}
type branchRef struct {
	Branch      string          `json:"branch"`
	Head        string          `json:"head,omitempty"`
	Base        string          `json:"base,omitempty"`
	PullRequest *pullRequestRef `json:"pullRequest,omitempty"`
}
type stack struct {
	ID       string      `json:"id,omitempty"`
	Number   int         `json:"number,omitempty"`
	Trunk    branchRef   `json:"trunk"`
	Branches []branchRef `json:"branches"`
}
type catalog struct {
	SchemaVersion int     `json:"schemaVersion"`
	Repository    string  `json:"repository"`
	Stacks        []stack `json:"stacks"`
}
type receipts struct {
	Frozen    map[string]bool    `json:"frozen,omitempty"`
	Submitted map[string]Version `json:"submitted,omitempty"`
}
type branchRow struct {
	branch         string
	parent         string
	parentRevision string
	ref            branchRef
	owner          int
}

func readCatalog(commonDir string) (catalog, error) {
	c := catalog{SchemaVersion: 1, Stacks: []stack{}}
	if err := readJSON(commonDir, "gh-stack", &c); err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	if c.SchemaVersion != 1 {
		return c, fmt.Errorf("native stack catalog has unsupported schema version %d", c.SchemaVersion)
	}
	return c, nil
}

func load(commonDir string) (catalog, receipts, error) {
	c, err := readCatalog(commonDir)
	if err != nil {
		return c, receipts{}, err
	}
	r := receipts{Frozen: map[string]bool{}, Submitted: map[string]Version{}}
	if err := readJSON(commonDir, "ccx-stack-receipts.json", &r); err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, r, err
	}
	return c, r, nil
}

func readJSON(commonDir, name string, value any) error {
	root, err := os.OpenRoot(commonDir)
	if err != nil {
		return fmt.Errorf("open metadata directory %s: %w", commonDir, err)
	}
	defer func() { _ = root.Close() }()
	data, err := root.ReadFile(name)
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Join(commonDir, name), err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Join(commonDir, name), err)
	}
	return nil
}

func rows(c catalog) ([]branchRow, error) {
	out := []branchRow{}
	seen := map[string]bool{}
	trunks := map[string]branchRef{}
	for i, s := range c.Stacks {
		parent := s.Trunk.Branch
		trunks[parent] = s.Trunk
		for _, b := range s.Branches {
			if seen[b.Branch] {
				return nil, fmt.Errorf("branch %s belongs to multiple native stacks", b.Branch)
			}
			seen[b.Branch] = true
			out = append(out, branchRow{branch: b.Branch, parent: parent, parentRevision: b.Base, ref: b, owner: i})
			parent = b.Branch
		}
	}
	for _, name := range slices.Sorted(maps.Keys(trunks)) {
		if !seen[name] {
			out = append(out, branchRow{branch: name, ref: trunks[name], owner: -1})
		}
	}
	return out, nil
}

// Init registers a trunk without replacing existing native stacks.
func Init(ctx context.Context, commonDir, trunk string) error {
	return mutate(ctx, commonDir, func(c *catalog, _ *receipts) (bool, bool, error) {
		for _, s := range c.Stacks {
			if s.Trunk.Branch == trunk {
				return false, false, nil
			}
		}
		out, err := render.RunCLI(ctx, render.Dir(commonDir), "git", []string{"--git-dir=" + commonDir, "rev-parse", "--verify", "refs/heads/" + trunk})
		if err != nil {
			return false, false, fmt.Errorf("resolve native stack trunk %s: %w", trunk, err)
		}
		c.Stacks = append(c.Stacks, stack{Trunk: branchRef{Branch: trunk, Head: strings.TrimSpace(out)}, Branches: []branchRef{}})
		return true, false, nil
	})
}

// Read combines the native catalog with current local branch heads.
func Read(ctx context.Context, commonDir string) (State, error) {
	s, _, err := readState(ctx, commonDir, false)
	return s, err
}

// ReadOrigin compares branch bases with the fetched origin trunk while leaving local trunk refs unchanged.
func ReadOrigin(ctx context.Context, commonDir string) (State, error) {
	s, _, err := ReadOriginOrphans(ctx, commonDir)
	return s, err
}

// ReadOriginOrphans also preserves children whose recorded parents no longer have refs.
func ReadOriginOrphans(ctx context.Context, commonDir string) (State, []Orphan, error) {
	return readState(ctx, commonDir, true)
}

func readState(ctx context.Context, commonDir string, remote bool) (State, []Orphan, error) {
	c, receipts, err := load(commonDir)
	if err != nil {
		return nil, nil, err
	}
	r, err := rows(c)
	if err != nil {
		return nil, nil, err
	}
	if len(r) == 0 {
		trunk, err := ReadTrunk(ctx, commonDir)
		if err != nil {
			return nil, nil, err
		}
		r = []branchRow{{branch: trunk, owner: -1}}
	}
	heads, err := readHeads(ctx, commonDir, r, remote)
	if err != nil {
		return nil, nil, err
	}
	state := State{}
	var orphans []Orphan
	for _, row := range r {
		head := heads[row.branch]
		if head == "" {
			continue
		}
		if row.parent == "" {
			state[row.branch] = BranchState{Trunk: true, Head: head}
			continue
		}
		parentHead := heads[row.parent]
		if parentHead == "" {
			orphans = append(orphans, Orphan{Branch: row.branch, Parent: row.parent, ParentRevision: row.parentRevision})
			continue
		}
		held := ""
		if receipts.Frozen[row.branch] {
			held = StateFrozen
		}
		state[row.branch] = BranchState{Head: head, State: held, NeedsRestack: row.parentRevision != parentHead, Parents: []Ref{{Ref: row.parent, SHA: row.parentRevision}}}
	}
	return state, orphans, nil
}

func readHeads(ctx context.Context, commonDir string, rows []branchRow, remote bool) (map[string]string, error) {
	refs := map[string]bool{}
	trunks := map[string]bool{}
	for _, r := range rows {
		refs["refs/heads/"+r.branch] = true
		if r.parent != "" {
			refs["refs/heads/"+r.parent] = true
		} else {
			trunks[r.branch] = true
		}
	}
	if remote {
		for trunk := range trunks {
			refs["refs/remotes/origin/"+trunk] = true
		}
	}
	if len(refs) == 0 {
		return map[string]string{}, nil
	}
	input := strings.Join(slices.Sorted(maps.Keys(refs)), "\n") + "\n"
	out, err := render.RunCLIStdin(ctx, render.Dir(commonDir), "git", []string{"--git-dir=" + commonDir, "for-each-ref", "--format=%(refname) %(objectname)", "--stdin"}, []byte(input))
	if err != nil {
		return nil, fmt.Errorf("read native stack branch heads: %w", err)
	}
	heads := map[string]string{}
	for line := range strings.Lines(out) {
		name, sha, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			heads[strings.TrimPrefix(name, "refs/heads/")] = sha
		}
	}
	if remote {
		for trunk := range trunks {
			if h := heads["refs/remotes/origin/"+trunk]; h != "" {
				heads[trunk] = h
			}
		}
	}
	return heads, nil
}

// ReadTrunk resolves a catalog trunk or the repository default before native initialization.
func ReadTrunk(ctx context.Context, commonDir string) (string, error) {
	c, _, err := load(commonDir)
	if err != nil {
		return "", err
	}
	trunks := map[string]bool{}
	branches := map[string]bool{}
	for _, s := range c.Stacks {
		trunks[s.Trunk.Branch] = true
		for _, b := range s.Branches {
			branches[b.Branch] = true
		}
	}
	for b := range branches {
		delete(trunks, b)
	}
	if len(trunks) == 1 {
		for trunk := range trunks {
			return trunk, nil
		}
	}
	out, err := render.RunCLI(ctx, render.Dir(commonDir), "git", []string{"--git-dir=" + commonDir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"})
	if err == nil {
		trunk := strings.TrimPrefix(strings.TrimSpace(out), "origin/")
		if len(trunks) == 0 || trunks[trunk] {
			return trunk, nil
		}
	}
	return "", fmt.Errorf("native stack catalog has no unambiguous trunk; initialize it with gh stack init --base <branch>")
}

// Rows includes stale catalog entries that normal state reads omit.
func Rows(ctx context.Context, commonDir string) ([]Row, error) {
	c, _, err := load(commonDir)
	if err != nil {
		return nil, err
	}
	r, err := rows(c)
	if err != nil {
		return nil, err
	}
	heads, err := readHeads(ctx, commonDir, r, false)
	if err != nil {
		return nil, err
	}
	out := make([]Row, 0, len(r))
	for _, b := range r {
		out = append(out, Row{Branch: b.branch, Parent: b.parent, Stale: heads[b.branch] == "", Diverged: b.parent != "" && heads[b.parent] == ""})
	}
	return out, nil
}

func mutate(ctx context.Context, commonDir string, change func(*catalog, *receipts) (bool, bool, error)) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	operation, err := lockCatalog(ctx, commonDir, "gh-stack-operation.lock")
	if err != nil {
		return err
	}
	defer func() { _ = operation.Close() }()
	lock, err := lockCatalog(ctx, commonDir, "gh-stack.lock")
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	c, r, err := load(commonDir)
	if err != nil {
		return err
	}
	catalogChanged, receiptsChanged, err := change(&c, &r)
	if err != nil {
		return err
	}
	if receiptsChanged {
		if err := writeJSON(commonDir, "ccx-stack-receipts.json", r); err != nil {
			return err
		}
	}
	if catalogChanged {
		return writeJSON(commonDir, "gh-stack", c)
	}
	return nil
}

func lockCatalog(ctx context.Context, commonDir, name string) (*os.File, error) {
	root, err := os.OpenRoot(commonDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := flockExclusive(ctx, f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock native stack catalog: %w", err)
	}
	return f, nil
}

func writeJSON(commonDir, name string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(commonDir, name+".tmp-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(commonDir, name))
}

// AdoptRoot records a branch under its actual parent and rejects a non-linear native stack.
func AdoptRoot(ctx context.Context, commonDir, branch, trunk, base, head string) error {
	return adoptRoot(ctx, commonDir, branch, trunk, base, head, false)
}

// AdoptFrozen records the same native relationship under a hold that prevents replay or publication.
func AdoptFrozen(ctx context.Context, commonDir, branch, trunk, base, head string) error {
	return adoptRoot(ctx, commonDir, branch, trunk, base, head, true)
}

func adoptRoot(ctx context.Context, commonDir, branch, parent, base, head string, frozen bool) error {
	return mutate(ctx, commonDir, func(c *catalog, r *receipts) (bool, bool, error) {
		all, err := rows(*c)
		if err != nil {
			return false, false, err
		}
		byName := map[string]branchRow{}
		for _, row := range all {
			byName[row.branch] = row
		}
		row, exists := byName[branch]
		if exists && row.parent == "" {
			return false, false, fmt.Errorf("cannot adopt native stack trunk %s", branch)
		}
		row.branch = branch
		row.parent = parent
		row.parentRevision = base
		row.ref.Branch = branch
		row.ref.Base = base
		row.ref.Head = head
		if !exists {
			row.owner = -1
		}
		byName[branch] = row
		if _, ok := byName[parent]; !ok {
			byName[parent] = branchRow{branch: parent, ref: branchRef{Branch: parent, Head: base}, owner: -1}
		}
		if err := rebuild(c, byName); err != nil {
			return false, false, err
		}
		if frozen {
			r.Frozen[branch] = true
		} else {
			delete(r.Frozen, branch)
		}
		return true, true, nil
	})
}

// Forget removes catalog entries and their receipts without deleting Git refs.
func Forget(ctx context.Context, commonDir string, branches []string) error {
	if len(branches) == 0 {
		return nil
	}
	return mutate(ctx, commonDir, func(c *catalog, r *receipts) (bool, bool, error) {
		drop := map[string]bool{}
		for _, b := range branches {
			drop[b] = true
			delete(r.Frozen, b)
			delete(r.Submitted, b)
		}
		for i := range c.Stacks {
			c.Stacks[i].Branches = slices.DeleteFunc(c.Stacks[i].Branches, func(b branchRef) bool { return drop[b.Branch] })
		}
		return true, true, nil
	})
}

// Reparent moves catalog relationships while retaining the fork points that still require replay.
func Reparent(ctx context.Context, commonDir string, moves map[string]string) error {
	if len(moves) == 0 {
		return nil
	}
	return mutate(ctx, commonDir, func(c *catalog, _ *receipts) (bool, bool, error) {
		all, err := rows(*c)
		if err != nil {
			return false, false, err
		}
		byName := map[string]branchRow{}
		for _, row := range all {
			byName[row.branch] = row
		}
		for branch, parent := range moves {
			row, ok := byName[branch]
			if !ok || row.parent == "" {
				return false, false, fmt.Errorf("native stack does not track branch %s", branch)
			}
			if _, ok := byName[parent]; !ok {
				return false, false, fmt.Errorf("native stack does not track parent %s", parent)
			}
			row.parent = parent
			byName[branch] = row
		}
		if err := rebuild(c, byName); err != nil {
			return false, false, err
		}
		return true, false, nil
	})
}

func rebuild(c *catalog, rows map[string]branchRow) error {
	children := map[string][]string{}
	for name, row := range rows {
		if row.parent != "" {
			children[row.parent] = append(children[row.parent], name)
		}
	}
	for parent, child := range children {
		if rows[parent].parent != "" && len(child) > 1 {
			return fmt.Errorf("native GitHub stacks are linear: %s would have multiple children", parent)
		}
	}
	var stacks []stack
	seen := map[string]bool{}
	identities := map[int]bool{}
	for _, trunk := range slices.Sorted(maps.Keys(rows)) {
		if rows[trunk].parent != "" {
			continue
		}
		firsts := slices.Clone(children[trunk])
		slices.Sort(firsts)
		if len(firsts) == 0 {
			stacks = append(stacks, stack{Trunk: rows[trunk].ref, Branches: []branchRef{}})
		}
		for _, first := range firsts {
			s := stack{Trunk: rows[trunk].ref, Branches: []branchRef{}}
			owner := rows[first].owner
			if owner >= 0 && !identities[owner] {
				s.ID = c.Stacks[owner].ID
				s.Number = c.Stacks[owner].Number
				identities[owner] = true
			}
			for name := first; name != ""; {
				if seen[name] {
					return fmt.Errorf("native stack contains cycle at %s", name)
				}
				seen[name] = true
				row := rows[name]
				row.ref.Base = row.parentRevision
				s.Branches = append(s.Branches, row.ref)
				name = ""
				if child := children[row.branch]; len(child) > 0 {
					name = child[0]
				}
			}
			stacks = append(stacks, s)
		}
	}
	for name, row := range rows {
		if row.parent != "" && !seen[name] {
			return fmt.Errorf("native stack contains cycle at %s", name)
		}
	}
	c.Stacks = stacks
	return nil
}

// LastSubmitted returns the publication receipts retained independently of the native catalog.
func LastSubmitted(_ context.Context, commonDir string) (map[string]Version, error) {
	_, r, err := load(commonDir)
	return r.Submitted, err
}

// RecordSubmitted updates a batch of publication receipts only when every branch is tracked.
func RecordSubmitted(ctx context.Context, commonDir string, versions map[string]Version) error {
	if len(versions) == 0 {
		return nil
	}
	return mutate(ctx, commonDir, func(c *catalog, r *receipts) (bool, bool, error) {
		all, err := rows(*c)
		if err != nil {
			return false, false, err
		}
		known := map[string]bool{}
		for _, row := range all {
			known[row.branch] = row.parent != ""
		}
		for b, v := range versions {
			if !known[b] {
				return false, false, fmt.Errorf("native stack does not track branch %s", b)
			}
			r.Submitted[b] = v
		}
		return false, true, nil
	})
}

// RecordRestacked advances recorded fork points after successful replay.
func RecordRestacked(ctx context.Context, commonDir string, revisions map[string]string) error {
	if len(revisions) == 0 {
		return nil
	}
	return mutate(ctx, commonDir, func(c *catalog, _ *receipts) (bool, bool, error) {
		remaining := maps.Clone(revisions)
		for i := range c.Stacks {
			for j := range c.Stacks[i].Branches {
				b := &c.Stacks[i].Branches[j]
				if revision, ok := remaining[b.Branch]; ok {
					b.Base = revision
					delete(remaining, b.Branch)
				}
			}
		}
		if len(remaining) > 0 {
			return false, false, fmt.Errorf("native stack does not track branch %s", slices.Sorted(maps.Keys(remaining))[0])
		}
		return true, false, nil
	})
}

// ReadPublishedChild combines a catalog relationship with its local head and publication receipt.
func ReadPublishedChild(ctx context.Context, commonDir, branch string) (PublishedChildState, error) {
	c, r, err := load(commonDir)
	if err != nil {
		return PublishedChildState{}, err
	}
	all, err := rows(c)
	if err != nil {
		return PublishedChildState{}, err
	}
	heads, err := readHeads(ctx, commonDir, all, false)
	if err != nil {
		return PublishedChildState{}, err
	}
	children := []string{}
	var found *branchRow
	for i := range all {
		if all[i].branch == branch {
			found = &all[i]
		}
		if all[i].parent == branch {
			children = append(children, all[i].branch)
		}
	}
	if found == nil {
		return PublishedChildState{}, fmt.Errorf("native stack does not track branch %s", branch)
	}
	encoded, err := json.Marshal(children)
	if err != nil {
		return PublishedChildState{}, err
	}
	out := PublishedChildState{Parent: found.parent, ParentRevision: found.parentRevision, BranchRevision: heads[branch], Validation: "VALID", Children: string(encoded)}
	if r.Frozen[branch] {
		out.State = StateFrozen
	}
	if v, ok := r.Submitted[branch]; ok {
		payload, err := json.Marshal(v)
		if err != nil {
			return PublishedChildState{}, err
		}
		out.Submitted = string(payload)
	}
	return out, nil
}

// PullRequest carries server identities to retain after successful native registration.
type PullRequest struct {
	Number      int
	ID          string
	URL         string
	StackNumber int
}

// RecordPullRequests attaches published PR identities to their existing native catalog branches.
func RecordPullRequests(ctx context.Context, commonDir string, published map[string]PullRequest) error {
	return mutate(ctx, commonDir, func(c *catalog, _ *receipts) (bool, bool, error) {
		found := map[string]bool{}
		for i := range c.Stacks {
			for j := range c.Stacks[i].Branches {
				branch := &c.Stacks[i].Branches[j]
				if pr, ok := published[branch.Branch]; ok {
					branch.PullRequest = &pullRequestRef{Number: pr.Number, ID: pr.ID, URL: pr.URL}
					if pr.StackNumber != 0 {
						c.Stacks[i].Number = pr.StackNumber
					}
					found[branch.Branch] = true
				}
			}
		}
		for branch := range published {
			if !found[branch] {
				return false, false, fmt.Errorf("native stack catalog does not track published branch %s", branch)
			}
		}
		return true, false, nil
	})
}

// BranchNames lists actual native catalog members without treating trunks as owned branches.
func BranchNames(commonDir string) ([]string, error) {
	c, err := readCatalog(commonDir)
	if err != nil {
		return nil, err
	}
	rows, err := rows(c)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, row := range rows {
		if row.parent != "" {
			names = append(names, row.branch)
		}
	}
	slices.Sort(names)
	return names, nil
}

// Membership reports catalog ownership independently of live refs or supplemental receipts.
func Membership(commonDir, branch string) (bool, error) {
	names, err := BranchNames(commonDir)
	return slices.Contains(names, branch), err
}
