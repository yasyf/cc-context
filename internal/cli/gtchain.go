package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type gtChainLink struct {
	name, parent, base, head string
	remote                   bool
}

func (l gtChainLink) segment() string { return "tracked " + l.name + " onto " + l.parent }

func gtUntrackedChain(ctx context.Context, dir render.Dir, prefix string, state gtState, trunk, base, branch, tip string) ([]gtChainLink, error) {
	floor, trunkFloor, remote := gtRestackRef(base), gtRestackRef(trunk), ""
	tr, err := gtTrunkRefOffline(ctx, dir, prefix, trunk)
	switch {
	case err == nil:
		remote, trunkFloor = tr.Remote(), string(tr.Ref())
		if base == trunk {
			floor = trunkFloor
		}
	case !errors.Is(err, vcs.ErrNoTrunk):
		return nil, err
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "--verify", tip})
	if err != nil {
		return nil, fmt.Errorf("%s: git rev-parse %s: %w", prefix, tip, err)
	}
	tipHead := strings.TrimSpace(out)
	if base != trunk {
		if floor, err = gtHeadUnder(ctx, dir, prefix, base, trunkFloor, tipHead); err != nil {
			return nil, err
		}
	}
	out, err = render.RunCLI(ctx, dir, "git", []string{"rev-list", "--parents", floor + ".." + tipHead})
	if err != nil {
		return nil, fmt.Errorf("%s: git rev-list %s..%s: %w", prefix, floor, tip, err)
	}
	firstParent := map[string]string{}
	inRange := map[string]bool{}
	for row := range strings.Lines(out) {
		shas := strings.Fields(row)
		inRange[shas[0]] = true
		if len(shas) > 1 {
			firstParent[shas[0]] = shas[1]
		}
	}
	at := map[string][]string{}
	remoteOnly := map[string]bool{}
	var level []string
	candidate := func(name, head string) {
		if _, tracked := state[name]; tracked || name == branch || !inRange[head] {
			return
		}
		if head == tipHead {
			level = append(level, name)
			return
		}
		at[head] = append(at[head], name)
	}
	locals, err := render.RunCLI(ctx, dir, "git", []string{"for-each-ref", "--format=%(objectname) %(refname:lstrip=2)", "refs/heads/"})
	if err != nil {
		return nil, fmt.Errorf("%s: git for-each-ref refs/heads/: %w", prefix, err)
	}
	local := map[string]bool{}
	for row := range strings.Lines(locals) {
		head, name, _ := strings.Cut(strings.TrimSpace(row), " ")
		local[name] = true
		candidate(name, head)
	}
	if remote != "" {
		heads, err := render.RunCLI(ctx, dir, "git", []string{"ls-remote", "--heads", remote})
		if err != nil {
			return nil, fmt.Errorf("%s: git ls-remote --heads %s: %w", prefix, remote, err)
		}
		for row := range strings.Lines(heads) {
			head, ref, _ := strings.Cut(strings.TrimSpace(row), "\t")
			name := strings.TrimPrefix(ref, "refs/heads/")
			if local[name] || strings.HasPrefix(name, "graphite-base/") || strings.HasPrefix(name, gtSegmentPrefix) {
				continue
			}
			remoteOnly[name] = true
			candidate(name, head)
		}
	}
	if len(level) > 0 {
		cut, err := gtLevelParent(ctx, dir, prefix, branch, remote, level, remoteOnly)
		if err != nil {
			return nil, err
		}
		if cut != "" {
			at[tipHead] = []string{cut}
		}
	}
	position := map[string]int{}
	for sha, i := tipHead, 0; inRange[sha]; sha, i = firstParent[sha], i+1 {
		position[sha] = i
	}
	var named []string
	ambiguous := false
	for head, names := range at {
		named = append(named, names...)
		_, onLine := position[head]
		ambiguous = ambiguous || !onLine || len(names) > 1
	}
	if ambiguous {
		slices.Sort(named)
		return nil, fmt.Errorf("%s: %s sits on untracked %s above %s, which form no single chain of branches — track each onto its parent with gt track <branch> --parent <parent>, bottom-up, then rerun", prefix, branch, strings.Join(named, ", "), base)
	}
	heads := slices.SortedFunc(maps.Keys(at), func(a, b string) int { return cmp.Compare(position[b], position[a]) })
	bottom := tipHead
	if len(heads) > 0 {
		bottom = heads[0]
	}
	fork, err := gtMergeBase(ctx, dir, prefix, bottom, floor)
	if err != nil {
		return nil, err
	}
	chain := make([]gtChainLink, 0, len(heads)+1)
	parent := base
	for _, head := range heads {
		name := at[head][0]
		chain = append(chain, gtChainLink{name: name, parent: parent, base: fork, head: head, remote: remoteOnly[name]})
		parent, fork = name, head
	}
	return append(chain, gtChainLink{name: branch, parent: parent, base: fork, head: tipHead}), nil
}

func gtLevelParent(ctx context.Context, dir render.Dir, prefix, branch, remote string, level []string, remoteOnly map[string]bool) (string, error) {
	from, err := gtCreatedFrom(ctx, dir, prefix, branch)
	if err != nil {
		return "", err
	}
	var unknown []string
	for _, name := range level {
		if from == name || remote != "" && from == remote+"/"+name {
			return name, nil
		}
		if remoteOnly[name] {
			unknown = append(unknown, name)
			continue
		}
		cut, err := gtCreatedFrom(ctx, dir, prefix, name)
		if err != nil {
			return "", err
		}
		if cut != branch {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return "", nil
	}
	slices.Sort(unknown)
	return "", fmt.Errorf("%s: %s sits at the same commit as untracked %s, and git's reflog does not say which was cut from which — track %s onto the branch it was cut from with gt track %s --parent <branch>, then rerun", prefix, branch, strings.Join(unknown, ", "), branch, branch)
}

func gtCreatedFrom(ctx context.Context, dir render.Dir, prefix, branch string) (string, error) {
	reflog := func(ref string) ([]string, error) {
		out, err := render.RunCLI(ctx, dir, "git", []string{"log", "--walk-reflogs", "--ignore-missing", "--format=%gs", ref, "--"})
		if err != nil {
			return nil, fmt.Errorf("%s: git log --walk-reflogs %s: %w", prefix, ref, err)
		}
		return strings.Split(strings.TrimSpace(out), "\n"), nil
	}
	entries, err := reflog("refs/heads/" + branch)
	if err != nil {
		return "", err
	}
	from, created := strings.CutPrefix(entries[len(entries)-1], "branch: Created from ")
	if !created {
		return "", nil
	}
	if from != "HEAD" {
		return from, nil
	}
	moves, err := reflog("HEAD")
	if err != nil {
		return "", err
	}
	from = ""
	for _, move := range moves {
		if moved, found := strings.CutPrefix(move, "checkout: moving from "); found {
			if source, target, _ := strings.Cut(moved, " to "); target == branch {
				from = source
			}
		}
	}
	return from, nil
}

func gtAdoptChain(ctx context.Context, dir render.Dir, prefix string, chain []gtChainLink) ([]string, error) {
	if len(chain) == 0 {
		return nil, nil
	}
	commonDir, err := gtCommonDir(ctx, dir, prefix)
	if err != nil {
		return nil, err
	}
	trunk, err := gtmeta.ReadTrunk(ctx, commonDir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", prefix, err)
	}
	segs := make([]string, 0, len(chain))
	for _, link := range chain {
		if link.remote {
			if _, err := render.RunCLI(ctx, dir, "git", []string{"branch", "--no-track", link.name, link.head}); err != nil {
				return nil, fmt.Errorf("%s: git branch %s %s: %w", prefix, link.name, shortOID(link.head), err)
			}
		}
		if err := gtmeta.AdoptRoot(ctx, commonDir, link.name, trunk, link.base, link.head); err != nil {
			return nil, fmt.Errorf("%s: %w", prefix, err)
		}
		if err := gtmeta.Reparent(ctx, commonDir, map[string]string{link.name: link.parent}); err != nil {
			return nil, fmt.Errorf("%s: %w", prefix, err)
		}
		segs = append(segs, link.segment())
	}
	return segs, nil
}

func gtAdoptBelow(ctx context.Context, dir render.Dir, prefix string, state gtState, base, branch string) (string, string, error) {
	trunk, err := gtTrunkBranch(prefix, state)
	if err != nil {
		return "", "", err
	}
	chain, err := gtUntrackedChain(ctx, dir, prefix, state, trunk, base, branch, gtRestackRef(branch))
	if err != nil {
		return "", "", err
	}
	below, tip := chain[:len(chain)-1], chain[len(chain)-1]
	segs, err := gtAdoptChain(ctx, dir, prefix, below)
	if err != nil {
		return "", "", err
	}
	chained := ""
	for _, seg := range segs {
		chained += seg + shipSep
	}
	return tip.parent, chained, nil
}

func gtHeadUnder(ctx context.Context, dir render.Dir, prefix, branch, floor, tip string) (string, error) {
	former, err := gtReflogHeads(ctx, dir, prefix, []string{branch})
	if err != nil {
		return "", err
	}
	held, err := gtNearestHeld(ctx, dir, prefix, former[branch], floor, tip)
	return cmp.Or(held, gtRestackRef(branch)), err
}

func gtNearestHeld(ctx context.Context, dir render.Dir, prefix string, held []string, floor, tip string) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-list", floor + ".." + tip})
	if err != nil {
		return "", fmt.Errorf("%s: git rev-list %s..%s: %w", prefix, floor, tip, err)
	}
	above := map[string]bool{}
	for _, commit := range strings.Fields(out) {
		above[commit] = true
	}
	nearest := ""
	for _, at := range held {
		if !above[at] || at == nearest {
			continue
		}
		if nearest != "" {
			ahead, err := gitIsAncestor(ctx, dir, prefix, nearest, at)
			if err != nil {
				return "", err
			}
			if !ahead {
				continue
			}
		}
		nearest = at
	}
	return nearest, nil
}

func gtMergeBase(ctx context.Context, dir render.Dir, prefix, a, b string) (string, error) {
	out, err := render.RunCLI(ctx, dir, "git", []string{"merge-base", a, b})
	if err != nil {
		return "", fmt.Errorf("%s: git merge-base %s %s: %w", prefix, a, b, err)
	}
	return strings.TrimSpace(out), nil
}
