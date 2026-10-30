package gtmeta

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/render"
)

const (
	// ProviderGitHub selects GitHub native catalog and stack APIs.
	ProviderGitHub = "github"
	// ProviderGraphite retains Graphite metadata and submission behavior.
	ProviderGraphite = "graphite"
)

type providerKey struct{}

// WithProvider binds a resolved backend so one planner cannot combine two provider forests.
func WithProvider(ctx context.Context, provider string) context.Context {
	if provider != ProviderGitHub && provider != ProviderGraphite {
		panic("invalid stack provider " + provider)
	}
	return context.WithValue(ctx, providerKey{}, provider)
}

func contextProvider(ctx context.Context) string {
	provider, _ := ctx.Value(providerKey{}).(string)
	return provider
}

type ownership struct {
	mode          string
	native        map[string]bool
	graphite      map[string]bool
	nativeStore   bool
	graphiteStore bool
}

func present(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func owners(ctx context.Context, commonDir string) (ownership, error) {
	o := ownership{mode: contextProvider(ctx), native: map[string]bool{}, graphite: map[string]bool{}}
	var err error
	o.nativeStore, err = present(filepath.Join(commonDir, "gh-stack"))
	if err != nil {
		return o, err
	}
	o.graphiteStore, err = present(filepath.Join(commonDir, repoConfig))
	if err != nil {
		return o, err
	}
	if o.nativeStore {
		names, err := nativemeta.BranchNames(commonDir)
		if err != nil {
			return o, err
		}
		for _, name := range names {
			o.native[name] = true
		}
	}
	if o.graphiteStore {
		trunk, err := graphiteReadTrunk(commonDir)
		if err != nil {
			return o, err
		}
		rows, err := readRows(ctx, filepath.Join(commonDir, metadataDB))
		if err != nil {
			return o, err
		}
		for _, row := range rows {
			if row.branch != trunk {
				o.graphite[row.branch] = true
			}
		}
	}
	return o, nil
}

// BranchProviders reads actual branch ownership with native catalog membership taking priority.
func BranchProviders(ctx context.Context, commonDir string) (map[string]string, error) {
	o, err := owners(ctx, commonDir)
	if err != nil {
		return nil, err
	}
	result := make(map[string]string, len(o.graphite)+len(o.native))
	for branch := range o.graphite {
		result[branch] = ProviderGraphite
	}
	for branch := range o.native {
		result[branch] = ProviderGitHub
	}
	return result, nil
}

// NativeMembership reports actual native branch ownership, excluding trunk entries.
func NativeMembership(commonDir, branch string) (bool, error) {
	return nativemeta.Membership(commonDir, branch)
}

// Provider preserves existing branch ownership before considering explicit mode or the native default.
func Provider(ctx context.Context, commonDir, branch string) (string, error) {
	native, err := NativeMembership(commonDir, branch)
	if err != nil {
		return "", err
	}
	if native {
		return ProviderGitHub, nil
	}
	legacy, err := present(filepath.Join(commonDir, repoConfig))
	if err != nil {
		return "", err
	}
	if legacy {
		trunk, err := graphiteReadTrunk(commonDir)
		if err != nil {
			return "", err
		}
		if branch != trunk {
			rows, err := readRows(ctx, filepath.Join(commonDir, metadataDB))
			if err != nil {
				return "", err
			}
			for _, row := range rows {
				if row.branch == branch {
					return ProviderGraphite, nil
				}
			}
		}
	}
	if provider := contextProvider(ctx); provider != "" {
		return provider, nil
	}
	mode, code, stderr, err := render.RunCLIExitCode(ctx, render.Dir(commonDir), "git", []string{"--git-dir=" + commonDir, "config", "--get", "ccx.stack"})
	if err != nil {
		return "", fmt.Errorf("read stack provider: %w", err)
	}
	if code == 1 {
		return ProviderGitHub, nil
	}
	if code != 0 {
		return "", fmt.Errorf("read stack provider: git config exited %d: %s", code, strings.TrimSpace(stderr))
	}
	switch mode = strings.TrimSpace(mode); mode {
	case ProviderGitHub, ProviderGraphite:
		return mode, nil
	default:
		return "", fmt.Errorf("ccx.stack %q is unsupported; choose github or graphite", mode)
	}
}

// Init initializes native metadata only when its trunk agrees with existing Graphite configuration.
func Init(ctx context.Context, commonDir, trunk string) error {
	legacy, err := present(filepath.Join(commonDir, repoConfig))
	if err != nil {
		return err
	}
	if legacy {
		previous, err := graphiteReadTrunk(commonDir)
		if err != nil {
			return err
		}
		if previous != trunk {
			return fmt.Errorf("native trunk %s differs from Graphite trunk %s", trunk, previous)
		}
	}
	return nativemeta.Init(ctx, commonDir, trunk)
}

func stores(commonDir string) (native, legacy bool, err error) {
	native, err = present(filepath.Join(commonDir, "gh-stack"))
	if err != nil {
		return
	}
	legacy, err = present(filepath.Join(commonDir, repoConfig))
	return
}

// Read isolates an explicitly selected provider or merges actual stores for generic inspection.
func Read(ctx context.Context, commonDir string) (State, error) {
	state, _, err := readProviders(ctx, commonDir, false)
	return state, err
}

// ReadOrigin evaluates the selected forest against fetched trunk heads without moving local refs.
func ReadOrigin(ctx context.Context, commonDir string) (State, error) {
	state, _, err := readProviders(ctx, commonDir, true)
	return state, err
}

// ReadOriginOrphans retains missing-parent evidence for the selected provider forest.
func ReadOriginOrphans(ctx context.Context, commonDir string) (State, []Orphan, error) {
	return readProviders(ctx, commonDir, true)
}

// ReadOriginOrphansForBranches resolves requested Graphite topology with a coherent native ownership snapshot.
func ReadOriginOrphansForBranches(ctx context.Context, commonDir string, branches []string) (State, []Orphan, error) {
	if len(branches) == 0 {
		panic("stack topology requires a branch")
	}
	if provider := contextProvider(ctx); provider != "" {
		if provider != ProviderGraphite {
			return readProviders(ctx, commonDir, true)
		}
		native, err := nativemeta.BranchNames(commonDir)
		if err != nil {
			return nil, nil, err
		}
		state, orphans, err := readSelectedStateOrphans(ctx, commonDir, true, branches, native)
		if err != nil {
			return nil, nil, err
		}
		return graphiteForest(state, orphans, native)
	}
	native, legacy, err := stores(commonDir)
	if err != nil {
		return nil, nil, err
	}
	if !native {
		return readSelectedStateOrphans(ctx, commonDir, true, branches, nil)
	}
	if legacy {
		if _, err := ReadTrunk(ctx, commonDir); err != nil {
			return nil, nil, err
		}
	}
	ns, no, names, err := nativemeta.ReadOriginOrphansWithMembers(ctx, commonDir)
	if err != nil {
		return nil, nil, err
	}
	state := State{}
	var orphans []Orphan
	if legacy {
		selected := append(slices.Clone(branches), names...)
		state, orphans, err = readSelectedStateOrphans(ctx, commonDir, true, selected, nil)
		if err != nil {
			return nil, nil, err
		}
	}
	for _, branch := range names {
		delete(state, branch)
	}
	orphans = slices.DeleteFunc(orphans, func(o Orphan) bool { return slices.Contains(names, o.Branch) })
	converted, missing := fromNative(ns, no)
	maps.Copy(state, converted)
	return state, append(orphans, missing...), nil
}

func readProviders(ctx context.Context, commonDir string, remote bool) (State, []Orphan, error) {
	if provider := contextProvider(ctx); provider != "" {
		if provider == ProviderGraphite {
			state, orphans, err := readStateOrphans(ctx, commonDir, remote)
			if err != nil {
				return nil, nil, err
			}
			native, err := nativemeta.BranchNames(commonDir)
			if err != nil {
				return nil, nil, err
			}
			return graphiteForest(state, orphans, native)
		}
		var native nativemeta.State
		var orphans []nativemeta.Orphan
		var err error
		if remote {
			native, orphans, err = nativemeta.ReadOriginOrphans(ctx, commonDir)
		} else {
			native, err = nativemeta.Read(ctx, commonDir)
		}
		if err != nil {
			return nil, nil, err
		}
		var trunks []string
		for branch, state := range native {
			if state.Trunk {
				trunks = append(trunks, branch)
			}
		}
		if len(trunks) > 1 {
			slices.Sort(trunks)
			return nil, nil, fmt.Errorf("native stacks target multiple trunks (%s); the shared stack planner requires one common trunk", strings.Join(trunks, ", "))
		}

		converted, missing := fromNative(native, orphans)
		return converted, missing, nil
	}
	native, legacy, err := stores(commonDir)
	if err != nil {
		return nil, nil, err
	}
	if !native {
		return readStateOrphans(ctx, commonDir, remote)
	}
	if legacy {
		if _, err := ReadTrunk(ctx, commonDir); err != nil {
			return nil, nil, err
		}
	}
	state := State{}
	var orphans []Orphan
	if legacy {
		state, orphans, err = readStateOrphans(ctx, commonDir, remote)
		if err != nil {
			return nil, nil, err
		}
	}
	var ns nativemeta.State
	var no []nativemeta.Orphan
	if remote {
		ns, no, err = nativemeta.ReadOriginOrphans(ctx, commonDir)
	} else {
		ns, err = nativemeta.Read(ctx, commonDir)
	}
	if err != nil {
		return nil, nil, err
	}
	names, err := nativemeta.BranchNames(commonDir)
	if err != nil {
		return nil, nil, err
	}
	for _, branch := range names {
		delete(state, branch)
	}
	orphans = slices.DeleteFunc(orphans, func(o Orphan) bool { return slices.Contains(names, o.Branch) })
	converted, missing := fromNative(ns, no)
	maps.Copy(state, converted)
	orphans = append(orphans, missing...)

	return state, orphans, nil
}

// ReadTrunk resolves the common trunk and rejects disagreement between installed providers.
func ReadTrunk(ctx context.Context, commonDir string) (string, error) {
	native, legacy, err := stores(commonDir)
	if err != nil {
		return "", err
	}
	if !native && contextProvider(ctx) != ProviderGitHub {
		return graphiteReadTrunk(commonDir)
	}
	trunk, err := nativemeta.ReadTrunk(ctx, commonDir)
	if err != nil {
		return "", err
	}
	if legacy {
		previous, err := graphiteReadTrunk(commonDir)
		if err != nil {
			return "", err
		}
		if previous != trunk {
			return "", fmt.Errorf("native trunk %s differs from Graphite trunk %s", trunk, previous)
		}
	}
	return trunk, nil
}

// Rows lists provider-owned pruning candidates without exposing native branches to a Graphite operation.
func Rows(ctx context.Context, commonDir string) ([]Row, error) {
	if contextProvider(ctx) == ProviderGraphite {
		rows, err := graphiteRows(ctx, commonDir)
		if err != nil {
			return nil, err
		}
		names, err := nativemeta.BranchNames(commonDir)
		if err != nil {
			return nil, err
		}
		rows = slices.DeleteFunc(rows, func(row Row) bool { return slices.Contains(names, row.Branch) })
		for _, row := range rows {
			if slices.Contains(names, row.Parent) {
				return nil, fmt.Errorf("cross-provider topology: Graphite branch %s depends on native branch %s; explicit stack conversion is required", row.Branch, row.Parent)
			}
		}
		return rows, nil
	}
	if contextProvider(ctx) == ProviderGitHub {
		native, err := nativemeta.Rows(ctx, commonDir)
		if err != nil {
			return nil, err
		}
		out := make([]Row, len(native))
		for i, row := range native {
			out[i] = Row(row)
		}
		return out, nil
	}
	native, legacy, err := stores(commonDir)
	if err != nil {
		return nil, err
	}
	if !native {
		return graphiteRows(ctx, commonDir)
	}
	byName := map[string]Row{}
	if legacy {
		rows, err := graphiteRows(ctx, commonDir)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			byName[row.Branch] = row
		}
	}
	rows, err := nativemeta.Rows(ctx, commonDir)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		byName[row.Branch] = Row(row)
	}
	out := make([]Row, 0, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		out = append(out, byName[name])
	}
	return out, nil
}

// AdoptRoot records a branch in its owning provider and rejects implicit cross-provider conversion.
func AdoptRoot(ctx context.Context, commonDir, branch, trunk, base, head string) error {
	return adoptProvider(ctx, commonDir, branch, trunk, base, head, false)
}

// AdoptFrozen records a held branch without changing the ownership of existing metadata.
func AdoptFrozen(ctx context.Context, commonDir, branch, trunk, base, head string) error {
	return adoptProvider(ctx, commonDir, branch, trunk, base, head, true)
}

func adoptProvider(ctx context.Context, commonDir, branch, parent, base, head string, frozen bool) error {
	o, err := owners(ctx, commonDir)
	if err != nil {
		return err
	}
	provider := contextProvider(ctx)
	switch {
	case o.native[branch]:
		if provider == ProviderGraphite {
			return fmt.Errorf("branch %s is native; explicit conversion is required before a Graphite write", branch)
		}
		provider = ProviderGitHub
	case o.graphite[branch]:
		if provider == ProviderGitHub {
			return fmt.Errorf("branch %s is Graphite; explicit conversion is required before a native write", branch)
		}
		provider = ProviderGraphite
	case provider != "":
	case o.nativeStore:
		provider = ProviderGitHub
	case o.graphiteStore:
		provider = ProviderGraphite
	default:
		provider, err = Provider(ctx, commonDir, branch)
		if err != nil {
			return err
		}
	}

	if parent != "" && (o.native[parent] || o.graphite[parent]) {
		parentProvider := ProviderGraphite
		if o.native[parent] {
			parentProvider = ProviderGitHub
		}
		if parentProvider != provider {
			return fmt.Errorf("cannot move %s from %s onto %s in %s without explicit stack conversion", branch, provider, parent, parentProvider)
		}
	}
	if provider == ProviderGitHub {
		if frozen {
			return nativemeta.AdoptFrozen(ctx, commonDir, branch, parent, base, head)
		}
		return nativemeta.AdoptRoot(ctx, commonDir, branch, parent, base, head)
	}
	if frozen {
		return graphiteAdoptFrozen(ctx, commonDir, branch, parent, base, head)
	}
	return graphiteAdoptRoot(ctx, commonDir, branch, parent, base, head)
}

func partition(o ownership, branches []string, requireTracked bool) (native, legacy []string, err error) {
	for _, branch := range branches {
		switch {
		case o.native[branch]:
			if o.mode == ProviderGraphite {
				return nil, nil, fmt.Errorf("branch %s is native; refusing Graphite write", branch)
			}
			native = append(native, branch)
		case o.graphite[branch]:
			if o.mode == ProviderGitHub {
				return nil, nil, fmt.Errorf("branch %s is Graphite; refusing native write", branch)
			}
			legacy = append(legacy, branch)
		case !requireTracked:
		case o.mode == ProviderGitHub:
			native = append(native, branch)
		case o.mode == ProviderGraphite:
			legacy = append(legacy, branch)
		case o.graphiteStore && !o.nativeStore:
			legacy = append(legacy, branch)
		case o.nativeStore && !o.graphiteStore:
			native = append(native, branch)
		default:
			return nil, nil, fmt.Errorf("stack metadata does not track branch %q", branch)
		}
	}
	return
}

// Forget removes each branch only from its owning metadata store, leaving Git refs untouched.
func Forget(ctx context.Context, commonDir string, branches []string) error {
	if len(branches) == 0 {
		return nil
	}
	o, err := owners(ctx, commonDir)
	if err != nil {
		return err
	}
	native, legacy, err := partition(o, branches, false)
	if err != nil {
		return err
	}
	if err := graphiteForget(ctx, commonDir, legacy); err != nil {
		return err
	}
	return nativemeta.Forget(ctx, commonDir, native)
}

// Reparent partitions relationship changes by ownership and refuses cross-provider parent edges.
func Reparent(ctx context.Context, commonDir string, moves map[string]string) error {
	if len(moves) == 0 {
		return nil
	}
	o, err := owners(ctx, commonDir)
	if err != nil {
		return err
	}
	native, legacy, err := partition(o, slices.Sorted(maps.Keys(moves)), true)
	if err != nil {
		return err
	}
	nativeMoves, legacyMoves := map[string]string{}, map[string]string{}
	for _, branch := range native {
		parent := moves[branch]
		if o.graphite[parent] && !o.native[parent] {
			return fmt.Errorf("cannot reparent native branch %s onto Graphite branch %s without explicit stack conversion", branch, parent)
		}
		nativeMoves[branch] = parent
	}
	for _, branch := range legacy {
		parent := moves[branch]
		if o.native[parent] {
			return fmt.Errorf("cannot reparent Graphite branch %s onto native branch %s without explicit stack conversion", branch, parent)
		}
		legacyMoves[branch] = parent
	}
	if err := graphiteReparent(ctx, commonDir, legacyMoves); err != nil {
		return err
	}
	return nativemeta.Reparent(ctx, commonDir, nativeMoves)
}

// ReadPublishedChild reads a child from its selected owner for publication-receipt comparison.
func ReadPublishedChild(ctx context.Context, commonDir, branch string) (PublishedChildState, error) {
	if contextProvider(ctx) == ProviderGraphite {
		native, err := NativeMembership(commonDir, branch)
		if err != nil {
			return PublishedChildState{}, err
		}
		if native {
			return PublishedChildState{}, fmt.Errorf("branch %s belongs to native GitHub stacks; refusing Graphite metadata read", branch)
		}
		return graphiteReadPublishedChild(ctx, commonDir, branch)
	}
	if contextProvider(ctx) == ProviderGitHub {
		row, err := nativemeta.ReadPublishedChild(ctx, commonDir, branch)
		return PublishedChildState(row), err
	}
	native, err := NativeMembership(commonDir, branch)
	if err != nil {
		return PublishedChildState{}, err
	}
	if !native {
		return graphiteReadPublishedChild(ctx, commonDir, branch)
	}
	row, err := nativemeta.ReadPublishedChild(ctx, commonDir, branch)
	return PublishedChildState(row), err
}

// LastSubmitted isolates or merges publication receipts with native ownership taking precedence.
func LastSubmitted(ctx context.Context, commonDir string) (map[string]Version, error) {
	if contextProvider(ctx) == ProviderGraphite {
		recorded, err := graphiteLastSubmitted(ctx, commonDir)
		if err != nil {
			return nil, err
		}
		names, err := nativemeta.BranchNames(commonDir)
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			delete(recorded, name)
		}
		return recorded, nil
	}
	if contextProvider(ctx) == ProviderGitHub {
		native, err := nativemeta.LastSubmitted(ctx, commonDir)
		if err != nil {
			return nil, err
		}
		out := map[string]Version{}
		for b, v := range native {
			out[b] = Version(v)
		}
		return out, nil
	}
	native, legacy, err := stores(commonDir)
	if err != nil {
		return nil, err
	}
	if !native {
		return graphiteLastSubmitted(ctx, commonDir)
	}
	out := map[string]Version{}
	if legacy {
		out, err = graphiteLastSubmitted(ctx, commonDir)
		if err != nil {
			return nil, err
		}
	}
	names, err := nativemeta.BranchNames(commonDir)
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		delete(out, name)
	}
	versions, err := nativemeta.LastSubmitted(ctx, commonDir)
	if err != nil {
		return nil, err
	}
	for branch, version := range versions {
		out[branch] = Version(version)
	}
	return out, nil
}

// RecordSubmitted partitions publication receipts without migrating branches between providers.
func RecordSubmitted(ctx context.Context, commonDir string, versions map[string]Version) error {
	if len(versions) == 0 {
		return nil
	}
	o, err := owners(ctx, commonDir)
	if err != nil {
		return err
	}
	native, legacy, err := partition(o, slices.Sorted(maps.Keys(versions)), true)
	if err != nil {
		return err
	}
	n, g := map[string]nativemeta.Version{}, map[string]Version{}
	for _, branch := range native {
		n[branch] = nativemeta.Version(versions[branch])
	}
	for _, branch := range legacy {
		g[branch] = versions[branch]
	}
	if err := graphiteRecordSubmitted(ctx, commonDir, g); err != nil {
		return err
	}
	return nativemeta.RecordSubmitted(ctx, commonDir, n)
}

// RecordRestacked updates fork points only in the providers that own the replayed branches.
func RecordRestacked(ctx context.Context, commonDir string, revisions map[string]string) error {
	if len(revisions) == 0 {
		return nil
	}
	o, err := owners(ctx, commonDir)
	if err != nil {
		return err
	}
	native, legacy, err := partition(o, slices.Sorted(maps.Keys(revisions)), true)
	if err != nil {
		return err
	}
	n, g := map[string]string{}, map[string]string{}
	for _, branch := range native {
		n[branch] = revisions[branch]
	}
	for _, branch := range legacy {
		g[branch] = revisions[branch]
	}
	if err := graphiteRecordRestacked(ctx, commonDir, g); err != nil {
		return err
	}
	return nativemeta.RecordRestacked(ctx, commonDir, n)
}

func fromNative(native nativemeta.State, orphans []nativemeta.Orphan) (State, []Orphan) {
	state := State{}
	for branch, b := range native {
		entry := BranchState{Trunk: b.Trunk, NeedsRestack: b.NeedsRestack, Head: b.Head, State: b.State}
		for _, parent := range b.Parents {
			entry.Parents = append(entry.Parents, Ref(parent))
		}
		state[branch] = entry
	}
	converted := make([]Orphan, len(orphans))
	for i, o := range orphans {
		converted[i] = Orphan(o)
	}
	return state, converted
}

func graphiteForest(state State, orphans []Orphan, native []string) (State, []Orphan, error) {
	for _, branch := range native {
		delete(state, branch)
	}
	orphans = slices.DeleteFunc(orphans, func(o Orphan) bool { return slices.Contains(native, o.Branch) })
	for branch, b := range state {
		for _, parent := range b.Parents {
			if slices.Contains(native, parent.Ref) {
				return nil, nil, fmt.Errorf("cross-provider topology: Graphite branch %s depends on native branch %s; explicit stack conversion is required", branch, parent.Ref)
			}
		}
	}
	for _, o := range orphans {
		if slices.Contains(native, o.Parent) {
			return nil, nil, fmt.Errorf("cross-provider topology: Graphite branch %s depends on native branch %s; explicit stack conversion is required", o.Branch, o.Parent)
		}
	}
	return state, orphans, nil
}
