package cli

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type stackPublicationTarget struct {
	Name string `json:"name"`
	Head string `json:"head"`
}

func stackPublicationTargets(plan []gtSubmitBranch) []stackPublicationTarget {
	targets := make([]stackPublicationTarget, len(plan))
	for i, branch := range plan {
		targets[i] = stackPublicationTarget{Name: branch.name, Head: branch.head}
	}
	return targets
}

func stackRecoverPublication(ctx context.Context, dir render.Dir, run *stackRebaseRun) error {
	if !run.Publishing || run.Pushed {
		return nil
	}
	if len(run.PushTargets) == 0 {
		return errors.New("stack publication: recovery has no saved push targets; retained replay pins")
	}
	matched, err := stackRemoteMatchesPublication(ctx, dir, "origin", run.PushTargets)
	if err != nil || !matched {
		return err
	}
	run.Pushed = true
	return stackSaveRun(run)
}

type stackPublication struct {
	Branch     string `json:"branch"`
	Source     string `json:"source"`
	SourceBase string `json:"source_base"`
	Head       string `json:"head"`
	Base       string `json:"base"`
	Parent     string `json:"parent"`
	OID        string `json:"receipt_oid,omitempty"`
}

func stackPublicationRef(branch, field string) string {
	return fmt.Sprintf("refs/ccx/published/%x/%s", sha256.Sum256([]byte(branch)), field)
}

func stackReadPublication(ctx context.Context, dir render.Dir, branch string) (*stackPublication, error) {
	ref := stackPublicationRef(branch, "receipt")
	present, err := gitRefExists(ctx, dir, stackRebasePrefix, ref)
	if err != nil || !present {
		return nil, err
	}
	oid, err := stackRevParse(ctx, dir, ref)
	if err != nil {
		return nil, err
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{"cat-file", "blob", oid})
	if err != nil {
		return nil, fmt.Errorf("stack publication: read %s: %w", ref, err)
	}
	var receipt stackPublication
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		return nil, fmt.Errorf("stack publication: decode %s: %w", ref, err)
	}
	if receipt.Branch != branch || receipt.Source == "" || receipt.SourceBase == "" || receipt.Head == "" || receipt.Base == "" || receipt.Parent == "" {
		return nil, fmt.Errorf("stack publication: incomplete receipt for %s", branch)
	}
	receipt.OID = oid
	return &receipt, nil
}

func stackUsePublication(ctx context.Context, dir render.Dir, b *stackRebaseBranch, receipt *stackPublication, pin string, published, below bool) error {
	if receipt == nil {
		return nil
	}
	was := b.WasParent
	b.Publication = receipt
	b.WasParent = receipt.Parent
	if b.Landed != "" {
		return nil
	}
	base, err := stackMergeBase(ctx, dir, receipt.Head, receipt.Base)
	if err != nil {
		return err
	}
	if b.Local == receipt.Head && b.Remote == receipt.Head {
		b.OldBase = base
		b.SourceBase = base
		return nil
	}
	if b.Remote == "" {
		b.Publication, b.WasParent = nil, was
		return nil
	}
	if b.Remote != receipt.Head {
		replayed, err := stackReplayedOnto(ctx, dir, b.Remote, base, receipt.Head)
		if err != nil {
			return err
		}
		if replayed == "" && b.Local == receipt.Source {
			if replayed, err = stackQueueRestackBase(ctx, dir, pin, b.Remote, receipt.Head); err != nil {
				return err
			}
		}
		if replayed == "" && published {
			b.Publication, b.WasParent = nil, was
			return nil
		}
		if replayed == "" && below {
			adopts, err := stackRemoteAdopts(ctx, dir, base, receipt.Head, b.Remote, pin)
			if err != nil {
				return err
			}
			if adopts {
				replayed = base
			}
		}
		if replayed == "" {
			return fmt.Errorf("stack rebase: %s's remote head %.12s holds commits this lane has never held; fetch and reconcile it before publishing again", b.Name, b.Remote)
		}
		base = replayed
	}
	if b.Local != receipt.Source {
		return nil
	}
	b.Head = b.Remote
	b.OldBase = base
	b.SourceBase = receipt.SourceBase
	return nil
}

// stackRemoteAdopts reports a remote head another lane pushed over this lane's
// publication of a branch below the one checked out that still sits on the
// publication's base and carries every commit it published, as a fast-forward
// does: building on it loses nothing.
func stackRemoteAdopts(ctx context.Context, dir render.Dir, base, published, remote, pin string) (bool, error) {
	onBase, err := gitIsAncestor(ctx, dir, stackRebasePrefix, base, remote)
	if err != nil || !onBase {
		return false, err
	}
	return gitOnlyCopies(ctx, dir, stackRebasePrefix, published, remote, pin)
}

func stackReplayedOnto(ctx context.Context, dir render.Dir, remote, ownBase, ownHead string) (string, error) {
	own, err := gtRevCount(ctx, stackRebasePrefix, dir, ownBase+".."+ownHead)
	if err != nil || own == 0 {
		return "", err
	}
	base, err := stackRevParse(ctx, dir, fmt.Sprintf("%s~%d", remote, own))
	if err != nil {
		return "", err
	}
	theirs, err := stackPatchSeries(ctx, dir, base, remote)
	if err != nil {
		return "", err
	}
	ours, err := stackPatchSeries(ctx, dir, ownBase, ownHead)
	if err != nil {
		return "", err
	}
	if theirs == nil || ours == nil || !slices.Equal(theirs, ours) {
		return "", nil
	}
	return base, nil
}

func stackPublicationState(state gtState, run *stackRebaseRun) gtState {
	state = maps.Clone(state)
	trunk := state[run.Trunk]
	trunk.Head = run.Pin
	state[run.Trunk] = trunk
	for _, b := range run.Branches {
		if b.Landed != "" {
			delete(state, b.Name)
			continue
		}
		entry := state[b.Name]
		entry.Head = b.NewHead
		entry.NeedsRestack = false
		entry.State = b.Held
		if b.Held == "" {
			entry.Parents = []gtRef{{Ref: b.Parent, SHA: b.NewBase}}
		}
		state[b.Name] = entry
	}
	return state
}

func stackCheckSources(ctx context.Context, dir render.Dir, run *stackRebaseRun) error {
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, b := range run.Branches {
		if run.leavesLocalRef(b) {
			continue
		}
		source := b.Local
		if b.LocalOnly && run.LocalApplied {
			source = b.NewHead
		}
		fmt.Fprintf(&tx, "verify %s %s\n", gtRestackRef(b.Name), source)
	}
	tx.WriteString("commit\n")
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(tx.String())); err != nil {
		return fmt.Errorf("stack publication: a source branch moved; all source refs and working copies were left untouched — ccx vcs stack abort drops the run: %w", err)
	}
	return nil
}

func stackPublicationPin(run *stackRebaseRun, branch string) string {
	return fmt.Sprintf("refs/ccx/publication-runs/%x", sha256.Sum256([]byte(run.dir+"\x00"+branch)))
}

func stackPinPublication(ctx context.Context, dir render.Dir, run *stackRebaseRun) error {
	if err := stackRecoverPublication(ctx, dir, run); err != nil {
		return err
	}
	if !run.Pushed {
		if err := stackCheckSources(ctx, dir, run); err != nil {
			return err
		}
	}
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, b := range run.Branches {
		if b.Landed == "" {
			fmt.Fprintf(&tx, "update %s %s\n", stackPublicationPin(run, b.Name), b.NewHead)
		}
	}
	tx.WriteString("commit\n")
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(tx.String())); err != nil {
		return fmt.Errorf("stack publication: pin replayed commits: %w", err)
	}
	return nil
}

func stackRecordPublication(ctx context.Context, dir render.Dir, run *stackRebaseRun, plan []gtSubmitBranch) error {
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, pushed := range plan {
		b := run.branch(pushed.name)
		receipt := stackPublication{Branch: b.Name, Source: b.Local, SourceBase: cmp.Or(b.SourceBase, b.OldBase), Head: pushed.head, Base: b.NewBase, Parent: pushed.base}
		if receipt.SourceBase == "" {
			return fmt.Errorf("stack publication: %s has no captured source base", b.Name)
		}
		expected := ""
		if b.Publication != nil {
			expected = b.Publication.OID
		}
		if err := stackReceiptTx(ctx, dir, &tx, receipt, expected); err != nil {
			return err
		}
	}
	tx.WriteString("commit\n")
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(tx.String())); err != nil {
		return fmt.Errorf("stack publication: record receipts; replay pins retained: %w", err)
	}
	run.Receipted = true
	return stackSaveRun(run)
}

func stackReceiptTx(ctx context.Context, dir render.Dir, tx *strings.Builder, receipt stackPublication, expected string) error {
	payload, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	out, err := render.RunCLIStdin(ctx, dir, "git", []string{"hash-object", "-w", "--stdin"}, payload)
	if err != nil {
		return fmt.Errorf("stack publication: write %s receipt: %w", receipt.Branch, err)
	}
	oid := strings.TrimSpace(out)
	prior, err := stackReadPublication(ctx, dir, receipt.Branch)
	if err != nil {
		return err
	}
	ref := stackPublicationRef(receipt.Branch, "receipt")
	switch {
	case prior != nil && prior.OID == oid:
		fmt.Fprintf(tx, "verify %s %s\n", ref, oid)
	case expected == "":
		fmt.Fprintf(tx, "create %s %s\n", ref, oid)
	default:
		fmt.Fprintf(tx, "update %s %s %s\n", ref, oid, expected)
	}
	for _, pin := range []struct{ name, oid string }{{"source", receipt.Source}, {"source-base", receipt.SourceBase}, {"head", receipt.Head}, {"base", receipt.Base}} {
		fmt.Fprintf(tx, "update %s %s\n", stackPublicationRef(receipt.Branch, pin.name), pin.oid)
	}
	return nil
}

func stackRemoteMatchesPublication(ctx context.Context, dir render.Dir, remote string, targets []stackPublicationTarget) (bool, error) {
	names := make([]string, len(targets))
	for i, target := range targets {
		names[i] = target.Name
	}
	heads, err := stackRemoteHeads(ctx, dir, stackRebasePrefix, remote, names, "")
	if err != nil {
		return false, err
	}
	for _, target := range targets {
		if heads[target.Name] != target.Head {
			return false, nil
		}
	}
	return true, nil
}

func stackPushPublication(ctx context.Context, dir render.Dir, s gtSubmit, plan []gtSubmitBranch) error {
	run := s.publication
	targets := stackPublicationTargets(plan)
	if run.Publishing && !slices.Equal(run.PushTargets, targets) {
		return errors.New("stack publication: push plan changed during recovery; retained original replay pins")
	}
	if err := thinRefuseAdoptedPush(ctx, dir, "stack publication", slices.Collect(maps.Keys(gtPushedHeads(plan)))); err != nil {
		return err
	}
	if err := stackRecoverPublication(ctx, dir, run); err != nil {
		return err
	}
	if !run.Pushed {
		if err := stackCheckSources(ctx, dir, run); err != nil {
			return err
		}
		run.PushTargets = targets
		run.Publishing = true
		if err := stackSaveRun(run); err != nil {
			return err
		}
		if err := gtRunPush(ctx, dir, s, plan); err != nil {
			if !gitPushStaleLease(err) {
				return gtPushFailure(s, plan, err)
			}
			matched, readErr := stackRemoteMatchesPublication(ctx, dir, "origin", targets)
			if readErr != nil {
				return errors.Join(gtPushFailure(s, plan, err), readErr)
			}
			if !matched {
				if err := stackRepushOverEquivalent(ctx, dir, s, run.Pin, plan, err); err != nil {
					return err
				}
			}
		}
		if err := thinRecordPush(ctx, dir, "origin", gtPushedHeads(plan)); err != nil {
			return err
		}
		run.Pushed = true
		return stackSaveRun(run)
	}
	matched, err := stackRemoteMatchesPublication(ctx, dir, "origin", targets)
	if err != nil {
		return err
	}
	if !matched {
		return errors.New("stack publication: remote heads changed after publication; retained the original receipts and source checkouts — ccx vcs stack abort drops the run")
	}
	return nil
}

// stackRepushOverEquivalent retries a push refused on a stale lease when every
// branch the remote moved now holds the same patches this run publishes, as
// the merge queue's own restack of a landed parent's children leaves them.
func stackRepushOverEquivalent(ctx context.Context, dir render.Dir, s gtSubmit, pin string, plan []gtSubmitBranch, pushErr error) error {
	names := make([]string, len(plan))
	for i, b := range plan {
		names[i] = b.name
	}
	heads, err := stackRemoteHeads(ctx, dir, stackRebasePrefix, "origin", names, pin)
	if err != nil {
		return errors.Join(gtPushFailure(s, plan, pushErr), err)
	}
	for i, b := range plan {
		remote := heads[b.name]
		if remote == b.lease {
			continue
		}
		if remote == "" {
			return gtPushFailure(s, plan, pushErr)
		}
		if remote == b.head {
			plan[i].lease, plan[i].leaseSet = remote, true
			continue
		}
		theirs, err := stackPatchSeries(ctx, dir, pin, remote)
		if err != nil {
			return err
		}
		ours, err := stackPatchSeries(ctx, dir, pin, b.head)
		if err != nil {
			return err
		}
		if theirs == nil || ours == nil || !slices.Equal(theirs, ours) {
			return gtPushFailure(s, plan, pushErr)
		}
		plan[i].lease, plan[i].leaseSet = remote, true
	}
	if err := gtRunPush(ctx, dir, s, plan); err != nil {
		return gtPushFailure(s, plan, err)
	}
	return nil
}

func stackCompletePublication(ctx context.Context, dir render.Dir, commonDir string, run *stackRebaseRun) error {
	if !run.Receipted {
		return errors.New("stack publication: receipts are incomplete; retained run and replay pins")
	}
	if err := stackDropPublicationPins(ctx, dir, run); err != nil {
		return err
	}
	return stackClearRun(commonDir, run)
}

func stackDropPublicationPins(ctx context.Context, dir render.Dir, run *stackRebaseRun) error {
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, b := range run.Branches {
		ref := stackPublicationPin(run, b.Name)
		at, err := render.RunCLI(ctx, dir, "git", []string{"for-each-ref", "--format=%(objectname)", ref})
		if err != nil {
			return fmt.Errorf("stack publication: read %s: %w", ref, err)
		}
		if at = strings.TrimSpace(at); at != "" {
			fmt.Fprintf(&tx, "delete %s %s\n", ref, at)
		}
	}
	tx.WriteString("commit\n")
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(tx.String())); err != nil {
		return fmt.Errorf("stack publication: release run pins: %w", err)
	}
	return nil
}

func stackFinishPublication(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun) error {
	if run.SourcesMoving {
		return stackFinishSourceMoves(ctx, cmd, l, commonDir, run)
	}
	if err := stackPinPublication(ctx, l.dir(), run); err != nil {
		return err
	}
	if run.deferPush {
		return nil
	}
	state, err := gtStateAtFocused(ctx, commonDir, stackRebasePrefix, run.Tip, "")
	if err != nil {
		return err
	}
	state = stackPublicationState(state, run)
	tr, err := gtTrunkRefOffline(ctx, l.dir(), stackRebasePrefix, run.Trunk)
	if err != nil {
		return err
	}
	var live []string
	for _, branch := range run.Branches {
		if branch.Landed == "" && branch.Held == "" && !branch.LocalOnly {
			live = append(live, branch.Name)
		}
	}
	if !run.Pushed && !run.LocalApplied {
		if err := stackCheckLocalOnly(ctx, l, run); err != nil {
			return err
		}
	}
	if !run.Publishing {
		if landed, err := stackLandedSince(ctx, l.dir(), run, live); err != nil {
			return err
		} else if len(landed) > 0 {
			return stackReplanLanded(ctx, cmd, l, commonDir, run, landed)
		}
	}
	heads := make([]string, len(live))
	for i, name := range live {
		heads[i] = run.branch(name).NewHead
	}
	commits, files, err := gtSubmitWidth(ctx, stackRebasePrefix, l.dir(), run.Pin, heads)
	if err != nil {
		return err
	}
	leases := stackPublicationLeases(run)
	sub := gtSubmit{prefix: stackRebasePrefix, suffix: " — source checkouts are untouched; run ccx vcs stack continue to resume publication", leases: leases, trunkHead: run.Pin, draft: run.Draft, draftAll: run.DraftAll, noVerify: run.NoVerify, publication: run}
	if run.Ship != nil {
		sub.prepared = run.Ship.Meta
	}
	published, entries, err := gtSubmitStack(ctx, l, cmd.ErrOrStderr(), sub, commonDir, state, tr, live, run.Tip)
	if err != nil {
		return err
	}
	cmd.Printf("published %d branches\nproposing %d commit(s), %d file(s)\n", len(published), commits, files)
	if run.Ship != nil {
		if err := stackFinishShip(ctx, cmd, l, run, entries); err != nil {
			return err
		}
	}
	strays, err := stackVerdict(ctx, cmd, l, run, live, published)
	if err != nil {
		return err
	}
	if err := stackMoveLocalOnly(ctx, cmd, l, commonDir, run); err != nil {
		return err
	}
	if err := stackFinishSourceMoves(ctx, cmd, l, commonDir, run); err != nil {
		return err
	}
	if len(strays) > 0 {
		return fmt.Errorf("%s: published, but GitHub still bases these pull requests off the parent the submit gave each: %s — finish each with the command its line names", stackRebasePrefix, strings.Join(strays, ", "))
	}
	return nil
}

func stackFinishSourceMoves(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun) error {
	report, err := stackMovePublishedSources(ctx, l, commonDir, run)
	if err != nil {
		return err
	}
	cmd.Println(report)
	return stackCompletePublication(ctx, l.dir(), commonDir, run)
}

func stackMovePublishedSources(ctx context.Context, l lane, commonDir string, run *stackRebaseRun) (string, error) {
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return "", fmt.Errorf("%s: %w", stackRebasePrefix, err)
	}
	var left []string
	if !run.SourcesMoving {
		if left, err = stackChooseSourceMoves(ctx, l, run, holders); err != nil {
			return "", err
		}
		run.SourcesMoving = true
		if err := stackSaveRun(run); err != nil {
			return "", err
		}
	}
	var moved []string
	var moves, pending []restackMove
	reparent := map[string]string{}
	revisions := map[string]string{}
	var tx strings.Builder
	tx.WriteString("start\n")
	for i := range run.Branches {
		b := &run.Branches[i]
		if b.Landed == "" && b.Held == "" {
			reparent[b.Name] = b.Parent
		}
		if !b.Moved {
			continue
		}
		at, err := stackRevParse(ctx, l.dir(), gtRestackRef(b.Name))
		if err != nil {
			return "", err
		}
		if at != b.Local && at != b.NewHead {
			b.Moved = false
			left = append(left, stackLeftSource(*b, "moved since the run started"))
			continue
		}
		receipt, err := stackReadPublication(ctx, l.dir(), b.Name)
		if err != nil {
			return "", err
		}
		onHead := stackPublication{Branch: b.Name, Source: receipt.Head, SourceBase: receipt.Base, Head: receipt.Head, Base: receipt.Base, Parent: receipt.Parent}
		if err := stackReceiptTx(ctx, l.dir(), &tx, onHead, receipt.OID); err != nil {
			return "", err
		}
		move := restackMove{branch: b.Name, head: b.NewHead, parent: b.NewBase, previous: b.Local}
		if at == b.NewHead {
			fmt.Fprintf(&tx, "verify %s %s\n", gtRestackRef(b.Name), b.NewHead)
		} else {
			fmt.Fprintf(&tx, "update %s %s %s\n", gtRestackRef(b.Name), b.NewHead, b.Local)
			pending = append(pending, move)
		}
		moved = append(moved, b.Name)
		moves = append(moves, move)
		revisions[b.Name] = b.NewBase
	}
	if len(moved) > 0 {
		if err := stackCheckPendingHolders(ctx, holders, pending); err != nil {
			return "", err
		}
		tx.WriteString("commit\n")
		if _, err := render.RunCLIStdin(ctx, l.dir(), "git", []string{"update-ref", "--stdin"}, []byte(tx.String())); err != nil {
			return "", fmt.Errorf("%s: the stack is published, but a source branch moved while its local ref was being moved onto its published head, so none was moved — ccx vcs stack continue retries it: %w", stackRebasePrefix, err)
		}
		if holders, err = vcs.BranchHolders(ctx, l.checkout); err != nil {
			return "", fmt.Errorf("%s: %w", stackRebasePrefix, err)
		}
		if _, err := gtRestackAlign(ctx, stackRebasePrefix, holders, moves); err != nil {
			return "", err
		}
	}
	if err := errors.Join(gtmeta.Reparent(ctx, commonDir, reparent), gtmeta.RecordRestacked(ctx, commonDir, revisions)); err != nil {
		return "", fmt.Errorf("%s: the stack is published, but recording its parents in gt failed — fix the cause and run ccx vcs stack continue: %w", stackRebasePrefix, err)
	}
	var segments []string
	if len(moved) > 0 {
		segments = append(segments, "moved "+strings.Join(moved, ", ")+" onto the published heads")
	}
	if len(left) > 0 {
		segments = append(segments, "kept local "+strings.Join(left, "; "))
	}
	if len(segments) == 0 {
		return "source checkouts unchanged", nil
	}
	return strings.Join(segments, shipSep), nil
}

// stackCheckPendingHolders re-checks, just before the refs move, each working
// copy that holds a branch still to move: a run resumed by ccx vcs stack continue
// chose its moves before it stopped, and the holder may have changed since.
func stackCheckPendingHolders(ctx context.Context, holders map[string]string, pending []restackMove) error {
	branches := make([]string, len(pending))
	for i, m := range pending {
		branches[i] = m.branch
	}
	if err := stackCheckClean(ctx, branches, holders, stackResumeAdvice); err != nil {
		return err
	}
	return gtRestackRefuseClobbers(ctx, stackRebasePrefix, holders, pending)
}

func stackChooseSourceMoves(ctx context.Context, l lane, run *stackRebaseRun, holders map[string]string) ([]string, error) {
	stays := map[string]bool{}
	var left []string
	for i := range run.Branches {
		b := &run.Branches[i]
		if b.Landed != "" || b.Held != "" || b.LocalOnly || b.NewHead == b.Local || run.leavesLocalRef(*b) {
			continue
		}
		at, err := stackRevParse(ctx, l.dir(), gtRestackRef(b.Name))
		if err != nil {
			return nil, err
		}
		receipt, err := stackReadPublication(ctx, l.dir(), b.Name)
		if err != nil {
			return nil, err
		}
		why := ""
		switch {
		case receipt == nil || receipt.Head != b.NewHead:
			why = "no receipt names its published head"
		case at != b.NewHead:
			if why, err = stackSourceStays(ctx, l, *b, at, holders, stays); err != nil {
				return nil, err
			}
		}
		if why != "" {
			stays[b.Name] = true
			left = append(left, stackLeftSource(*b, why))
			continue
		}
		b.Moved = true
	}
	return left, nil
}

func stackLeftSource(b stackRebaseBranch, why string) string {
	return fmt.Sprintf("%s at %.12s instead of its published head %.12s (%s)", b.Name, b.Local, b.NewHead, why)
}

func stackSourceStays(ctx context.Context, l lane, b stackRebaseBranch, at string, holders map[string]string, stays map[string]bool) (string, error) {
	if stays[b.Parent] {
		return "stacked on " + b.Parent, nil
	}
	if at != b.Local {
		return "moved since the run started", nil
	}
	holder := holders[b.Name]
	if holder == "" {
		return "", nil
	}
	if !b.Resolved {
		replays, err := stackPublishedReplaysSource(ctx, l.dir(), b)
		if err != nil {
			return "", err
		}
		if !replays {
			return "its published head carries other changes", nil
		}
	}
	dirty, err := gitUncommitted(ctx, holder)
	if err != nil {
		return "", fmt.Errorf("%s: git status in %s: %w", stackRebasePrefix, holder, err)
	}
	if dirty {
		return "uncommitted work in " + holder, nil
	}
	clobbered, err := gtRestackClobbers(ctx, stackRebasePrefix, holder, restackMove{branch: b.Name, head: b.NewHead, previous: b.Local})
	if err != nil {
		return "", err
	}
	if len(clobbered) > 0 {
		return "the published head tracks " + strings.Join(clobbered, ", ") + ", which " + holder + " ignores", nil
	}
	return "", nil
}

func stackPublishedReplaysSource(ctx context.Context, dir render.Dir, b stackRebaseBranch) (bool, error) {
	sourceBase := cmp.Or(b.SourceBase, b.OldBase)
	source, err := stackPatchSeries(ctx, dir, sourceBase, b.Local)
	if err != nil {
		return false, err
	}
	published, err := stackPatchSeries(ctx, dir, b.NewBase, b.NewHead)
	if err != nil {
		return false, err
	}
	if source == nil || published == nil {
		return false, nil
	}
	if slices.Equal(source, published) {
		return true, nil
	}
	unmatched := source
	for _, commit := range published {
		_, message, _ := strings.Cut(commit, "\n")
		i := slices.IndexFunc(unmatched, func(c string) bool {
			_, m, _ := strings.Cut(c, "\n")
			return m == message
		})
		if i < 0 {
			return false, nil
		}
		unmatched = unmatched[i+1:]
	}
	merged, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"--attr-source=" + b.NewBase, "merge-tree", "--write-tree", "--merge-base=" + sourceBase, b.NewBase, b.Local})
	if err != nil {
		return false, fmt.Errorf("%s: git merge-tree %.12s %.12s: %w", stackRebasePrefix, b.NewBase, b.Local, err)
	}
	switch code {
	case 0:
	case 1:
		return false, nil
	default:
		return false, fmt.Errorf("%s: git merge-tree %.12s %.12s: exit %d: %s", stackRebasePrefix, b.NewBase, b.Local, code, strings.TrimSpace(stderr))
	}
	tree, err := render.RunCLI(ctx, dir, "git", []string{"rev-parse", b.NewHead + "^{tree}"})
	if err != nil {
		return false, fmt.Errorf("%s: git rev-parse %.12s^{tree}: %w", stackRebasePrefix, b.NewHead, err)
	}
	return strings.TrimSpace(strings.SplitN(merged, "\n", 2)[0]) == strings.TrimSpace(tree), nil
}

func stackLocalOnlyMoves(run *stackRebaseRun) []restackMove {
	var moves []restackMove
	for _, b := range run.Branches {
		if b.LocalOnly && b.NewHead != b.Local {
			moves = append(moves, restackMove{branch: b.Name, head: b.NewHead, parent: b.NewBase, previous: b.Local})
		}
	}
	return moves
}

func stackCheckLocalOnly(ctx context.Context, l lane, run *stackRebaseRun) error {
	moves := stackLocalOnlyMoves(run)
	if len(moves) == 0 {
		return nil
	}
	movers := make([]string, len(moves))
	for i, m := range moves {
		movers[i] = m.branch
	}
	holders, err := vcs.BranchHolders(ctx, l.checkout)
	if err != nil {
		return fmt.Errorf("%s: %w", stackRebasePrefix, err)
	}
	if err := stackCheckClean(ctx, movers, holders, stackResumeAdvice); err != nil {
		return err
	}
	return gtRestackRefuseClobbers(ctx, stackRebasePrefix, holders, moves)
}

func stackMoveLocalOnly(ctx context.Context, cmd *cobra.Command, l lane, commonDir string, run *stackRebaseRun) error {
	reparent := map[string]string{}
	revisions := map[string]string{}
	for _, b := range run.Branches {
		if b.LocalOnly {
			revisions[b.Name] = b.NewBase
			if b.Parent != b.WasParent {
				reparent[b.Name] = b.Parent
			}
		}
	}
	if len(revisions) == 0 {
		return nil
	}
	if !run.LocalApplied {
		if err := stackCheckLocalOnly(ctx, l, run); err != nil {
			return err
		}
		run.LocalApplied = true
		if err := stackSaveRun(run); err != nil {
			return err
		}
	}
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, m := range stackLocalOnlyMoves(run) {
		at, err := stackRevParse(ctx, l.dir(), gtRestackRef(m.branch))
		if err != nil {
			return err
		}
		if at == m.head {
			fmt.Fprintf(&tx, "verify %s %s\n", gtRestackRef(m.branch), m.head)
		} else {
			fmt.Fprintf(&tx, "update %s %s %s\n", gtRestackRef(m.branch), m.head, m.previous)
		}
	}
	tx.WriteString("commit\n")
	if _, err := render.RunCLIStdin(ctx, l.dir(), "git", []string{"update-ref", "--stdin"}, []byte(tx.String())); err != nil {
		return fmt.Errorf("%s: the stack is published, but a branch without a pull request moved locally since the run started, so it was left where it is — ccx vcs stack continue retries it: %w", stackRebasePrefix, err)
	}
	if !run.LocalAligned {
		holders, err := vcs.BranchHolders(ctx, l.checkout)
		if err != nil {
			return fmt.Errorf("%s: %w", stackRebasePrefix, err)
		}
		if _, err := gtRestackAlign(ctx, stackRebasePrefix, holders, stackLocalOnlyMoves(run)); err != nil {
			return err
		}
		run.LocalAligned = true
		if err := stackSaveRun(run); err != nil {
			return err
		}
	}
	if err := errors.Join(gtmeta.Reparent(ctx, commonDir, reparent), gtmeta.RecordRestacked(ctx, commonDir, revisions)); err != nil {
		return fmt.Errorf("%s: the local branches are rebased, but recording them in gt failed — fix the cause and run ccx vcs stack continue: %w", stackRebasePrefix, err)
	}
	cmd.Printf("rebased %s locally · not pushed (no pull request)\n", strings.Join(slices.Sorted(maps.Keys(revisions)), ", "))
	return nil
}

func stackPublicationLeases(run *stackRebaseRun) map[string]string {
	leases := make(map[string]string, len(run.Branches))
	for _, branch := range run.Branches {
		leases[branch.Name] = branch.Remote
	}
	return leases
}

func stackHasPublication(ctx context.Context, dir render.Dir, branches []string) (bool, error) {
	for _, branch := range branches {
		present, err := gitRefExists(ctx, dir, stackRebasePrefix, stackPublicationRef(branch, "receipt"))
		if err != nil {
			return false, err
		}
		if present {
			return true, nil
		}
	}
	return false, nil
}
