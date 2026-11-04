package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type stackNewOpts struct {
	parent      string
	path        string
	published   bool
	sparse      bool
	noCheckout  bool
	thin        bool
	fullHistory bool
	deepen      bool
	depth       int
	depthSet    bool
	maxDepth    int
	includes    []string
}

type stackSparse struct {
	patterns []byte
	cone     bool
	dirs     []string
}

func runStackNew(cmd *cobra.Command, name string, options stackNewOpts) error {
	ctx := cmd.Context()
	if options.parent != "" {
		ctx = context.WithValue(ctx, stackParentKey{}, options.parent)
	}
	src, err := resolveLane(ctx, "stack new", workingDir(ctx), false)
	if err != nil {
		return err
	}
	ctx = withStackProvider(ctx, src.native)
	cmd.SetContext(ctx)
	current, err := gitCurrentBranch(ctx, src.dir(), "stack new")
	if err != nil {
		return err
	}
	parent := cmp.Or(options.parent, current)
	if parent == "" {
		return errors.New("stack new: HEAD is detached here, so there is no branch to stack on — check one out, or name it with --parent")
	}
	storage, err := stackStorageOf(ctx, options, src, parent)
	if err != nil {
		return err
	}
	inStore, err := thinIsStore(ctx, src.checkout)
	if err != nil {
		return fmt.Errorf("stack new: %w", err)
	}
	if storage == storageFullHistory && inStore {
		return fmt.Errorf("stack new: %s is a thin store, so no lane cut here carries full history — run --full-history from a full checkout of the repository", src.root)
	}
	thin := storage == storageThin && !inStore
	sparseLane := options.sparse || (storage == storageThin || inStore || len(options.includes) > 0) && !options.noCheckout
	if !sparseLane && !options.noCheckout && src.checkout.Kind == vcs.Git {
		if sparseLane, err = laneSparseByDefault(ctx, src.dir()); err != nil {
			return err
		}
	}
	if options.published && !src.gt {
		return errors.New("stack new: --published-parent requires a stack-managed lane")
	}
	if (options.sparse || options.noCheckout || storage == storageThin) && src.checkout.Kind != vcs.Git {
		return errors.New("stack new: sparse, no-checkout, and thin creation require a Git checkout")
	}
	if options.noCheckout && thin {
		return errors.New("stack new: a thin lane is a sparse checkout, so --no-checkout conflicts with thin storage; pass --full-history from a full checkout")
	}
	if len(options.includes) > 0 && (options.noCheckout || src.checkout.Kind != vcs.Git) {
		return errors.New("stack new: --include checks out directories in a sparse lane, which needs a Git checkout and no --no-checkout")
	}
	if (options.depthSet || options.deepen) && !thin {
		return errors.New("stack new: --depth and --deepen apply only to a thin lane cut from a full checkout")
	}
	if options.depth < 1 || options.maxDepth < 1 {
		return fmt.Errorf("stack new: --depth %d and --max-depth %d must both be positive", options.depth, options.maxDepth)
	}
	var sparse *stackSparse
	switch {
	case options.sparse:
		read, err := stackReadSparse(ctx, src.dir())
		if err != nil {
			return err
		}
		if thin || inStore {
			if read, err = stackWithMetadata(ctx, src.dir(), read); err != nil {
				return err
			}
		}
		sparse = &read
	case sparseLane:
		cone, err := laneSparse(ctx, src.dir())
		if err != nil {
			return err
		}
		sparse = &cone
	}
	if inStore {
		if err := thinVerifyNotes(ctx, src, src.checkout.MainRoot); err != nil {
			return err
		}
	}
	l := src
	var segs []string
	var adopted *stackPublication
	if thin {
		s, err := thinSourceOf(ctx, src)
		if err != nil {
			return err
		}
		store, created, err := thinEnsureStore(ctx, cmd.ErrOrStderr(), src, s, options)
		if err != nil {
			return err
		}
		if created {
			segs = append(segs, "created thin store "+store.root)
		}
		l = store
		if adopted, err = stackThinParent(ctx, src, store, name, parent, s.trunk); err != nil {
			return err
		}
		if adopted != nil {
			deepened, err := thinAdopt(ctx, src, store, adopted, s.trunk, options)
			if err != nil {
				return err
			}
			if deepened > 0 {
				segs = append(segs, fmt.Sprintf("deepened %s by %d commits to reach %s", store.root, deepened, shortOID(adopted.Base)))
			}
		}
	}
	trunk := sync.OnceValues(func() (string, error) { return stackNewTrunk(ctx, l) })
	path, err := stackNewPath(ctx, l.checkout, name, options.path, src.checkout)
	if err != nil {
		return err
	}
	var start string
	var receipt *stackPublication
	var common string
	switch {
	case adopted != nil:
		start = adopted.Head
	case options.published:
		common, err = gtCommonDir(ctx, l.dir(), "stack new")
		if err != nil {
			return err
		}
		receipt, err = stackReadPublication(ctx, l.dir(), parent)
		if err != nil {
			return err
		}
		if receipt == nil {
			return fmt.Errorf("stack new: %s has no publication receipt; publish it first", parent)
		}
		if err := stackVerifyNewParent(ctx, l.dir(), receipt); err != nil {
			return err
		}
		if err := stackVerifyNewRemote(ctx, l.dir(), receipt); err != nil {
			return err
		}
		start = receipt.Head
	default:
		t, err := trunk()
		if err != nil {
			return err
		}
		tracked, err := stackNewParent(ctx, l, parent, t)
		if err != nil {
			return err
		}
		segs = append(segs, tracked...)
		start, err = stackNewStart(ctx, l, parent, t)
		if err != nil {
			return err
		}
	}
	base, err := stackRevParse(ctx, l.dir(), start)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	args := []string{"worktree", "add"}
	if sparse != nil || options.noCheckout {
		args = append(args, "--no-checkout")
	}
	args = append(args, "-b", name, path, base)
	if _, err := render.RunCLI(ctx, l.dir(), "git", args); err != nil {
		return fmt.Errorf("stack new: create %s: %w", path, err)
	}
	created := render.Dir(path)
	finish := func() error {
		if sparse != nil {
			head, err := stackRevParse(ctx, created, "HEAD")
			if err != nil {
				return err
			}
			if want := stackNewPinned(receipt, adopted); want != "" && head != want {
				return errors.New("stack new: child ref changed before sparse population")
			}
			if err := stackWriteSparse(ctx, created, "stack new", *sparse, head); err != nil {
				return err
			}
			if len(options.includes) > 0 {
				if err := stackAddCone(ctx, "stack new", created, options.includes); err != nil {
					return err
				}
			}
		}
		if adopted != nil {
			return stackFinishAdopted(ctx, cmd.ErrOrStderr(), src, l, created, path, name, adopted)
		}
		if receipt == nil {
			if err := stackFormLane(ctx, cmd.ErrOrStderr(), l, created, name, parent); err != nil {
				return stackUndoNew(ctx, l, path, name, parent, err)
			}
			return nil
		}
		if l.checkout.Kind == vcs.JJ {
			if err := stackColocateJJ(ctx, created, name); err != nil {
				return err
			}
		}
		if err := stackTrackPublished(ctx, created, cmd.ErrOrStderr(), common, name, receipt); err != nil {
			return err
		}
		if err := stackVerifyNewParent(ctx, l.dir(), receipt); err != nil {
			return err
		}
		head, err := stackRevParse(ctx, created, "HEAD")
		if err != nil {
			return err
		}
		if head != receipt.Head {
			return errors.New("stack new: child ref changed during creation")
		}
		return nil
	}
	if err := finish(); err != nil {
		if errors.Is(err, errStackNewUndone) {
			return err
		}
		return fmt.Errorf("stack new: child %s at %s is incomplete; its worktree, ref, and metadata were retained for inspection: %w", name, path, err)
	}
	if err := recordLane(ctx, "stack new", path); err != nil {
		return err
	}
	joins, err := stackNewJoinsLane(ctx, l, current, parent, trunk)
	if err != nil {
		return err
	}
	if joins {
		if err := laneJoin("stack new", src, path); err != nil {
			return err
		}
	}
	segs = append(segs, "cut "+name+" onto "+parent+" at "+shortOID(base))
	if sparse != nil {
		segs = append(segs, laneWiden(path))
	}
	here := "this tree stays detached"
	if current != "" {
		here = "this tree stays on " + current
	}
	cmd.Println(here + "; work in " + path)
	cmd.Println(strings.Join(append(segs, path), shipSep))
	return nil
}

func stackNewPinned(receipts ...*stackPublication) string {
	for _, r := range receipts {
		if r != nil {
			return r.Head
		}
	}
	return ""
}

func stackThinParent(ctx context.Context, src, store lane, name, parent, trunk string) (*stackPublication, error) {
	if parent == trunk {
		return nil, nil
	}
	present, err := gitRefExists(ctx, store.dir(), "stack new", gtRestackRef(parent))
	if err != nil {
		return nil, err
	}
	adopted, err := thinAdopted(ctx, store, parent)
	if err != nil {
		return nil, err
	}
	if present && !adopted {
		return nil, stackThinSameParent(ctx, src, store, parent)
	}
	fullHistory := fmt.Sprintf("ccx vcs stack new %s --parent %s --full-history", name, parent)
	if !src.gt {
		return nil, fmt.Errorf("stack new: %s lives outside thin store %s, and only the graphite lane brings a parent's publication in; cut the lane from this checkout's full history with %s", parent, store.root, fullHistory)
	}
	receipt, err := stackReadPublication(ctx, src.dir(), parent)
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		return nil, fmt.Errorf("stack new: %s lives outside thin store %s and %s has no publication to bring in; publish it with ccx vcs stack submit from its working copy and rerun, or cut the lane from this checkout's full history with %s", parent, store.root, parent, fullHistory)
	}
	if err := stackVerifyNewParent(ctx, src.dir(), receipt); err != nil {
		return nil, err
	}
	if err := stackVerifyNewRemote(ctx, src.dir(), receipt); err != nil {
		return nil, err
	}
	return receipt, nil
}

func stackThinSameParent(ctx context.Context, src, store lane, parent string) error {
	ref := gtRestackRef(parent)
	inSource, err := gitRefExists(ctx, src.dir(), "stack new", ref)
	if err != nil || !inSource {
		return err
	}
	want, err := stackRevParse(ctx, src.dir(), ref)
	if err != nil {
		return err
	}
	got, err := stackRevParse(ctx, store.dir(), ref)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("stack new: thin store %s has its own %s at %s, but %s here is at %s; nothing changed — cut the lane from the store's checkout of %s, or create it with --full-history", store.root, parent, shortOID(got), parent, shortOID(want), parent)
	}
	return nil
}

func stackFinishAdopted(ctx context.Context, errW io.Writer, src, store lane, created render.Dir, path, name string, receipt *stackPublication) error {
	if err := stackFormLane(ctx, errW, store, created, name, receipt.Branch); err != nil {
		return stackUndoNew(ctx, store, path, name, receipt.Branch, err)
	}
	if err := gtmeta.RecordRestacked(ctx, store.checkout.CommonDir, map[string]string{name: receipt.Head}); err != nil {
		return err
	}
	if err := stackVerifyNewParent(ctx, src.dir(), receipt); err != nil {
		return err
	}
	head, err := stackRevParse(ctx, created, "HEAD")
	if err != nil {
		return err
	}
	if head != receipt.Head {
		return errors.New("stack new: child ref changed during creation")
	}
	return nil
}

var errStackNewUndone = errors.New("stack new: removed")

// stackUndoNew removes a lane gt refused to adopt, whose branch and worktree
// hold nothing yet: left behind, the next stack new refuses on the existing
// destination, and a ship from it adopts the branch onto whatever tracked
// ancestor gt finds instead of the parent named.
func stackUndoNew(ctx context.Context, l lane, path, name, parent string, cause error) error {
	if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"worktree", "remove", "--force", path}); err != nil {
		return fmt.Errorf("stack new: gt could not adopt %s onto %s, and removing %s failed: %w (adoption: %w)", name, parent, path, err, cause)
	}
	if _, err := render.RunCLI(ctx, l.dir(), "git", []string{"branch", "-D", name}); err != nil {
		return fmt.Errorf("stack new: gt could not adopt %s onto %s, and deleting the branch failed: %w (adoption: %w)", name, parent, err, cause)
	}
	return fmt.Errorf("%w %s and its worktree, since gt could not adopt it onto %s: %w", errStackNewUndone, name, parent, cause)
}

func stackNewPath(ctx context.Context, checkout vcs.Checkout, name, requested string, source vcs.Checkout) (string, error) {
	var path string
	var err error
	if requested == "" {
		path, err = mintGitWorktreePath(ctx, "stack new", checkout, name)
		if err != nil {
			return "", err
		}
	} else {
		if _, err := worktreeName("stack new", name); err != nil {
			return "", err
		}
		if !filepath.IsAbs(requested) {
			requested = filepath.Join(workingDir(ctx), requested)
		}
		path, err = filepath.Abs(requested)
		if err != nil {
			return "", err
		}
		path, err = canonicalNewWorktreePath(path)
		if err != nil {
			return "", err
		}
	}
	for _, root := range []string{checkout.Root, checkout.MainRoot, source.Root, source.MainRoot} {
		if root == "" {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return "", err
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("stack new: destination %s is inside checkout %s", path, root)
		}
	}
	if err := refuseUsedLane(ctx, "stack new", checkout, name, path); err != nil {
		return "", err
	}
	if _, err := os.Lstat(path); err == nil {
		return "", fmt.Errorf("stack new: destination %s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return path, nil
}

// stackNewJoinsLane reports whether a child cut onto parent carries on the
// lane checked out at current: parent is current, or shares its gt stack.
func stackNewJoinsLane(ctx context.Context, l lane, current, parent string, trunk func() (string, error)) (bool, error) {
	if current == "" {
		return false, nil
	}
	t, err := trunk()
	if err != nil {
		return false, err
	}
	if parent == t || current == t {
		return false, nil
	}
	if parent == current || !l.gt {
		return parent == current, nil
	}
	state, err := gtStateQuery(ctx, l.dir(), "stack new")
	if err != nil {
		return false, err
	}
	return gtSameStack(state, t, parent, current), nil
}

// laneJoin puts the working copy at path into src's lane, so stack submit from
// either one treats the branches the other holds as its own.
func laneJoin(prefix string, src lane, path string) error {
	created, err := vcs.ResolveCheckout(path)
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	if err := vcs.JoinLane(src.checkout, created); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	return nil
}

func gtSameStack(state gtState, trunk, a, b string) bool {
	return gtDescends(state, trunk, a, b) || gtDescends(state, trunk, b, a)
}

func gtDescends(state gtState, trunk, branch, ancestor string) bool {
	for range len(state) + 1 {
		switch branch {
		case ancestor:
			return true
		case trunk:
			return false
		}
		s, ok := state[branch]
		if !ok || len(s.Parents) == 0 {
			return false
		}
		branch = s.Parents[0].Ref
	}
	return false
}

func stackNewStart(ctx context.Context, l lane, parent, trunk string) (string, error) {
	if parent != trunk {
		return parent, nil
	}
	tr, err := gtTrunkRef(ctx, l.dir(), "stack new", trunk)
	if err != nil {
		return "", err
	}
	return gtTrunkHead(ctx, l.dir(), "stack new", tr)
}

func stackNewParent(ctx context.Context, l lane, parent, trunk string) ([]string, error) {
	if !l.gt || parent == trunk {
		return nil, nil
	}
	if l.native {
		member, err := gtmeta.NativeMembership(l.checkout.CommonDir, parent)
		if err != nil || member {
			return nil, err
		}
	}
	present, err := gitRefExists(ctx, l.dir(), "stack new", gtRestackRef(parent))
	if err != nil {
		return nil, err
	}
	if !present {
		if present, err = gtBranchFromRemote(ctx, l.dir(), "stack new", "origin", parent); err != nil || !present {
			return nil, err
		}
	}
	return gtAdoptUntracked(ctx, l.dir(), "stack new", parent)
}

func stackNewTrunk(ctx context.Context, l lane) (string, error) {
	if !l.gt {
		remote, err := vcs.GitRemoteFor(ctx, l.dir(), "HEAD")
		if err != nil {
			return "", fmt.Errorf("stack new: %w", err)
		}
		tr, err := vcs.ResolveTrunk(ctx, l.dir(), remote)
		if err != nil {
			return "", fmt.Errorf("stack new: %w", err)
		}
		return tr.Name(), nil
	}
	state, err := gtStateQuery(ctx, l.dir(), "stack new")
	if err != nil {
		return "", err
	}
	return gtTrunkBranch("stack new", state)
}

func stackConfigBool(ctx context.Context, dir render.Dir, key string, worktree bool) (bool, error) {
	args := []string{"config"}
	if worktree {
		args = append(args, "--worktree")
	}
	args = append(args, "--bool", "--get", key)
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", args)
	if err != nil {
		return false, err
	}
	if code == 1 {
		return false, nil
	}
	if code != 0 {
		return false, fmt.Errorf("stack new: read %s: %s", key, strings.TrimSpace(stderr))
	}
	return strconv.ParseBool(strings.TrimSpace(out))
}

func stackReadSparse(ctx context.Context, dir render.Dir) (stackSparse, error) {
	sparse, active, err := stackActiveSparse(ctx, dir)
	if err != nil {
		return stackSparse{}, err
	}
	if !active {
		return stackSparse{}, errors.New("stack new: --sparse requires active per-worktree sparse configuration in the caller")
	}
	return sparse, nil
}

// laneSparseByDefault reports whether a lane cut from dir is sparse unasked:
// only once ccx.sparseLanes is true, and then only when dir tracks more than
// laneSparseFiles files at HEAD, since a smaller repository gains nothing from
// a cone and loses every source dir.
func laneSparseByDefault(ctx context.Context, dir render.Dir) (bool, error) {
	on, err := stackConfigBool(ctx, dir, sparseLanesKey, false)
	if err != nil || !on {
		return false, err
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{"ls-tree", "-r", "--name-only", "-z", "HEAD"})
	if err != nil {
		return false, err
	}
	return strings.Count(out, "\x00") > laneSparseFiles, nil
}

// laneSparse is the cone a lane cut from dir checks out by default: dir's own
// active sparse set, else root files alone, plus laneConeDirs and each
// ccx.sparseInclude directory.
func laneSparse(ctx context.Context, dir render.Dir) (stackSparse, error) {
	inherited, err := stackThinSparse(ctx, dir)
	if err != nil {
		return stackSparse{}, err
	}
	return stackWithMetadata(ctx, dir, inherited)
}

// laneIncludes is every directory ccx.sparseInclude adds to a sparse lane cut
// from dir, in the order git config lists them.
func laneIncludes(ctx context.Context, dir render.Dir) ([]string, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"config", "--get-all", sparseIncludeKey})
	if err != nil {
		return nil, err
	}
	if code == 1 {
		return nil, nil
	}
	if code != 0 {
		return nil, fmt.Errorf("stack new: read %s: %s", sparseIncludeKey, strings.TrimSpace(stderr))
	}
	var dirs []string
	for line := range strings.Lines(out) {
		if include := strings.TrimSpace(line); include != "" {
			dirs = append(dirs, include)
		}
	}
	return dirs, nil
}

// laneWiden is the summary segment naming the command that checks out one
// more directory of the sparse lane at path.
func laneWiden(path string) string {
	return "widen with git -C " + path + " sparse-checkout add <dir>"
}

func stackThinSparse(ctx context.Context, dir render.Dir) (stackSparse, error) {
	sparse, active, err := stackActiveSparse(ctx, dir)
	if err != nil || active {
		return sparse, err
	}
	return stackSparse{patterns: []byte("/*\n!/*/\n"), cone: true}, nil
}

func stackWithMetadata(ctx context.Context, dir render.Dir, s stackSparse) (stackSparse, error) {
	includes, err := laneIncludes(ctx, dir)
	if err != nil {
		return stackSparse{}, err
	}
	patterns := slices.Clone(s.patterns)
	have := map[string]bool{}
	for line := range strings.Lines(string(patterns)) {
		have[strings.TrimSpace(line)] = true
	}
	for _, dir := range laneConeDirs {
		pattern := "/" + dir + "/"
		if have[pattern] {
			continue
		}
		if len(patterns) > 0 && patterns[len(patterns)-1] != '\n' {
			patterns = append(patterns, '\n')
		}
		patterns = append(patterns, pattern+"\n"...)
	}
	return stackSparse{patterns: patterns, cone: s.cone, dirs: slices.Concat(s.dirs, includes)}, nil
}

func stackActiveSparse(ctx context.Context, dir render.Dir) (stackSparse, bool, error) {
	perWorktree, err := stackConfigBool(ctx, dir, "extensions.worktreeConfig", false)
	if err != nil || !perWorktree {
		return stackSparse{}, false, err
	}
	active, err := stackConfigBool(ctx, dir, "core.sparseCheckout", true)
	if err != nil || !active {
		return stackSparse{}, false, err
	}
	cone, err := stackConfigBool(ctx, dir, "core.sparseCheckoutCone", true)
	if err != nil {
		return stackSparse{}, false, err
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "--path-format=absolute", "--git-path", "info/sparse-checkout"})
	if err != nil {
		return stackSparse{}, false, err
	}
	patterns, err := os.ReadFile(strings.TrimSpace(out))
	if err != nil {
		return stackSparse{}, false, err
	}
	return stackSparse{patterns: patterns, cone: cone}, true, nil
}

func stackWriteSparse(ctx context.Context, dir render.Dir, prefix string, sparse stackSparse, head string) error {
	if err := stackEnableWorktreeConfig(ctx, dir, prefix); err != nil {
		return err
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "--path-format=absolute", "--git-path", "info/sparse-checkout"})
	if err != nil {
		return err
	}
	path := strings.TrimSpace(out)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(path, sparse.patterns, 0o600); err != nil {
		return err
	}
	for _, setting := range [][2]string{{"core.sparseCheckout", "true"}, {"core.sparseCheckoutCone", strconv.FormatBool(sparse.cone)}} {
		if _, err := render.RunCLI(ctx, dir, "git", []string{"config", "--worktree", setting[0], setting[1]}); err != nil {
			return err
		}
	}
	if err := stackPopulateSparse(ctx, dir, prefix, head); err != nil {
		return err
	}
	if len(sparse.dirs) == 0 {
		return nil
	}
	return stackAddCone(ctx, prefix, dir, sparse.dirs)
}

// stackEnableWorktreeConfig turns on extensions.worktreeConfig the way git's
// own sparse-checkout does, moving any shared core.bare or core.worktree out of
// the common config, so the new worktree's sparse settings stay its own.
func stackEnableWorktreeConfig(ctx context.Context, dir render.Dir, prefix string) error {
	enabled, err := stackConfigBool(ctx, dir, "extensions.worktreeConfig", false)
	if err != nil || enabled {
		return err
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"-c", "core.hooksPath=/dev/null", "sparse-checkout", "set", "--cone", "--no-sparse-index"}); err != nil {
		return fmt.Errorf("%s: enable per-worktree config in %s: %w", prefix, dir, err)
	}
	return nil
}

func stackPopulateSparse(ctx context.Context, dir render.Dir, prefix, head string) error {
	out, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", "--path-format=absolute", "--git-path", "index"})
	if err != nil {
		return err
	}
	index := strings.TrimSpace(out)
	scratch, err := os.MkdirTemp(filepath.Dir(index), "ccx-sparse-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	privateTree := filepath.Join(scratch, "tree")
	privateIndex := filepath.Join(scratch, "index")
	if err := os.Mkdir(privateTree, 0o700); err != nil {
		return err
	}
	extraEnv := []string{"GIT_WORK_TREE=" + privateTree, "GIT_INDEX_FILE=" + privateIndex}
	if _, err := render.RunCLIEnv(ctx, dir, "git", []string{"-c", "core.splitIndex=false", "read-tree", "-m", "-u", head}, extraEnv); err != nil {
		return fmt.Errorf("%s: prepare the sparse index of %s: %w", prefix, dir, err)
	}
	return stackInstallSparseIndex(ctx, dir, prefix, privateIndex, index)
}

func stackInstallSparseIndex(ctx context.Context, dir render.Dir, prefix, privateIndex, index string) (err error) {
	lockPath := index + ".lock"
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // lockPath is the new worktree's Git index lock
	if err != nil {
		return fmt.Errorf("%s: acquire the index lock of %s: %w", prefix, dir, err)
	}
	defer func() { err = errors.Join(err, lock.Close(), os.Remove(lockPath)) }()
	if err := os.Link(privateIndex, index); err != nil {
		return fmt.Errorf("%s: the index of %s already exists or could not be installed: %w", prefix, dir, err)
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"checkout-index", "--all"}); err != nil {
		return fmt.Errorf("%s: populate %s without overwriting files: %w", prefix, dir, err)
	}
	return nil
}

func stackVerifyNewParent(ctx context.Context, dir render.Dir, receipt *stackPublication) error {
	source, err := stackRevParse(ctx, dir, gtRestackRef(receipt.Branch))
	if err != nil {
		return err
	}
	if source != receipt.Source {
		return fmt.Errorf("stack new: %s source changed since publication; publish it before creating a published child", receipt.Branch)
	}
	tx := fmt.Sprintf("start\nverify %s %s\nverify %s %s\ncommit\n", gtRestackRef(receipt.Branch), receipt.Source, stackPublicationRef(receipt.Branch, "receipt"), receipt.OID)
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(tx)); err != nil {
		return fmt.Errorf("stack new: parent changed during child creation: %w", err)
	}
	return nil
}

func stackTrackPublished(ctx context.Context, dir render.Dir, errW io.Writer, common, name string, receipt *stackPublication) error {
	result, runErr := gtRun(ctx, dir, []string{"track", "--force", "--no-interactive"}, errW)
	if err := gtReport(ctx, errW, result); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("stack new: track published child: %w", runErr)
	}
	if err := gtmeta.Reparent(ctx, common, map[string]string{name: receipt.Branch}); err != nil {
		return err
	}
	return gtmeta.RecordRestacked(ctx, common, map[string]string{name: receipt.Head})
}

func stackVerifyNewRemote(ctx context.Context, dir render.Dir, receipt *stackPublication) error {
	remote, err := vcs.GitRemoteFor(ctx, dir, gtRestackRef(receipt.Branch))
	if err != nil {
		return err
	}
	out, err := runRemoteGit(ctx, dir, []string{"ls-remote", "--heads", remote, gtRestackRef(receipt.Branch)})
	if err != nil {
		return err
	}
	head := ""
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == gtRestackRef(receipt.Branch) {
			head = fields[0]
		}
	}
	if head != receipt.Head {
		return fmt.Errorf("stack new: %s remote changed after publication; expected %s, found %s", receipt.Branch, receipt.Head, head)
	}
	return nil
}
