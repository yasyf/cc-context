package cli

import (
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
)

type stackTopology struct {
	ctx          context.Context
	dir          render.Dir
	commonDir    string
	state        gtState
	retargeted   gtState
	overrides    map[string]string
	rewound      map[string]string
	nearestState gtState
	complete     bool
	expanded     map[string]bool
	queried      map[string]bool
}

func (t *stackTopology) include(names ...string) error {
	return t.load(gtmeta.ReadOriginOrphansForAncestors, names)
}

func (t *stackTopology) load(read func(context.Context, string, []string) (gtmeta.State, []gtmeta.Orphan, error), names []string) error {
	if t.complete {
		return nil
	}
	var missing []string
	for _, name := range names {
		if name == "" || t.queried[name] {
			continue
		}
		if _, known := t.state[name]; known {
			continue
		}
		t.queried[name] = true
		missing = append(missing, name)
	}
	if len(missing) == 0 {
		return nil
	}
	fresh, err := gtRequestedState(t.ctx, t.commonDir, stackRebasePrefix, missing, read)
	if err != nil {
		return err
	}
	return t.merge(fresh)
}

func (t *stackTopology) expand(names ...string) error {
	if t.complete {
		return nil
	}
	var requested []string
	for _, name := range names {
		if name != "" && !t.expanded[name] {
			t.expanded[name] = true
			requested = append(requested, name)
		}
	}
	if len(requested) == 0 {
		return nil
	}
	fresh, err := gtRequestedState(t.ctx, t.commonDir, stackRebasePrefix, requested, gtmeta.ReadOriginOrphansForBranches)
	if err != nil {
		return err
	}
	return t.merge(fresh)
}

func (t *stackTopology) merge(fresh gtState) error {
	var added []string
	for name, entry := range fresh {
		if _, known := t.state[name]; known {
			continue
		}
		t.state[name] = entry
		added = append(added, name)
	}
	if len(added) == 0 {
		return nil
	}
	slices.Sort(added)
	claims, err := stackClaimRewound(t.ctx, t.dir, t.state, added)
	if err != nil {
		return err
	}
	maps.Copy(t.rewound, claims)
	newState := gtState{}
	for _, name := range added {
		newState[name] = t.state[name]
	}
	if t.retargeted != nil {
		maps.Copy(t.retargeted, stackRetargeted(newState, t.overrides))
	}
	return nil
}

func (t *stackTopology) infer(parent string) (string, error) {
	if t.nearestState == nil {
		if t.complete {
			t.nearestState = t.state
		} else {
			state, err := gtStateAt(t.ctx, t.commonDir, stackRebasePrefix)
			if err != nil {
				return "", err
			}
			t.nearestState = state
		}
	}
	if !t.complete {
		maps.Copy(t.nearestState, t.state)
	}
	below, err := gtInferParent(t.ctx, &gtCache{dir: t.dir, prefix: stackRebasePrefix, commonDir: t.commonDir, state: t.nearestState}, parent)
	if err != nil {
		return "", err
	}
	if err := t.include(below); err != nil {
		return "", err
	}
	return below, nil
}

func stackRequestedNames(o stackRebaseOpts) []string {
	names := slices.Concat(o.members, o.linearize, o.landed, o.newPRs, o.pinned, o.include, o.bump)
	names = append(names, o.tip, o.to, o.above)
	for _, pair := range o.parents {
		if child, parent, ok := strings.Cut(pair, "="); ok {
			names = append(names, child, parent)
		}
	}
	for child, recorded := range o.recorded {
		names = append(names, child, recorded.branch, recorded.parent)
	}
	for name := range o.replayed {
		names = append(names, name)
	}
	if o.target != nil {
		names = append(names, o.target.branch)
	}
	return names
}
