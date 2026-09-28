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

func stackUsePublication(ctx context.Context, dir render.Dir, b *stackRebaseBranch, receipt *stackPublication) error {
	if receipt == nil {
		return nil
	}
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
	if b.Remote != receipt.Head {
		replayed, err := stackReplayedOnto(ctx, dir, b.Remote, base, receipt.Head)
		if err != nil {
			return err
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
	b.HeadRef = stackTempRef(b.Name)
	b.OldBase = base
	b.SourceBase = receipt.SourceBase
	return nil
}

func stackReplayedOnto(ctx context.Context, dir render.Dir, remote, ownBase, ownHead string) (string, error) {
	own, err := gtRevCount(ctx, stackRebasePrefix, dir, ownBase+".."+ownHead)
	if err != nil {
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
	heads, err := stackRemoteHeads(ctx, dir, remote, names, "")
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
		if _, err := render.RunCLI(ctx, dir, "git", gtPushArgv(s, plan)); err != nil {
			if !gitPushStaleLease(err) {
				return gtPushFailure(s, plan, err)
			}
			matched, readErr := stackRemoteMatchesPublication(ctx, dir, "origin", targets)
			if readErr != nil || !matched {
				return errors.Join(gtPushFailure(s, plan, err), readErr)
			}
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

func stackCompletePublication(ctx context.Context, dir render.Dir, commonDir string, run *stackRebaseRun) error {
	if !run.Receipted {
		return errors.New("stack publication: receipts are incomplete; retained run and replay pins")
	}
	if err := stackDropPublicationPins(ctx, dir, run); err != nil {
		return err
	}
	if err := stackDropTempRefs(ctx, dir, run); err != nil {
		return err
	}
	return stackClearRun(commonDir, run)
}

func stackDropPublicationPins(ctx context.Context, dir render.Dir, run *stackRebaseRun) error {
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, b := range run.Branches {
		ref := stackPublicationPin(run, b.Name)
		present, err := gitRefExists(ctx, dir, stackRebasePrefix, ref)
		if err != nil {
			return err
		}
		if present {
			fmt.Fprintf(&tx, "delete %s %s\n", ref, b.NewHead)
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
		if landed, err := stackLandedSince(ctx, l.dir(), run.Trunk, live); err != nil {
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
	sub := gtSubmit{prefix: stackRebasePrefix, suffix: " — source checkouts are untouched; run ccx vcs stack continue to resume publication", leases: leases, trunkHead: run.Pin, draft: run.Draft, noVerify: run.NoVerify, publication: run}
	_, entries, err := gtSubmitStack(ctx, l, cmd.ErrOrStderr(), sub, commonDir, state, tr, live, "")
	if err != nil {
		return err
	}
	cmd.Printf("published %d branches\nproposing %d commit(s), %d file(s)\n", len(live), commits, files)
	if run.Ship != nil {
		if err := stackFinishShip(ctx, cmd, l, run, entries); err != nil {
			return err
		}
	}
	if err := stackVerdict(ctx, cmd, l.dir(), run, live); err != nil {
		return err
	}
	if err := stackMoveLocalOnly(ctx, cmd, l, commonDir, run); err != nil {
		return err
	}
	return stackFinishSourceMoves(ctx, cmd, l, commonDir, run)
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
	var moves []restackMove
	reparent := map[string]string{}
	revisions := map[string]string{}
	var tx strings.Builder
	tx.WriteString("start\n")
	for i := range run.Branches {
		b := &run.Branches[i]
		if !b.Moved {
			continue
		}
		at, err := stackRevParse(ctx, l.dir(), gtRestackRef(b.Name))
		if err != nil {
			return "", err
		}
		if at != b.Local && at != b.NewHead {
			b.Moved = false
			left = append(left, b.Name+" (moved since the run started)")
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
		if at == b.NewHead {
			fmt.Fprintf(&tx, "verify %s %s\n", gtRestackRef(b.Name), b.NewHead)
		} else {
			fmt.Fprintf(&tx, "update %s %s %s\n", gtRestackRef(b.Name), b.NewHead, b.Local)
		}
		moved = append(moved, b.Name)
		moves = append(moves, restackMove{branch: b.Name, head: b.NewHead, parent: b.NewBase, previous: b.Local})
		revisions[b.Name] = b.NewBase
		reparent[b.Name] = b.Parent
	}
	if len(moved) > 0 {
		tx.WriteString("commit\n")
		if _, err := render.RunCLIStdin(ctx, l.dir(), "git", []string{"update-ref", "--stdin"}, []byte(tx.String())); err != nil {
			return "", fmt.Errorf("%s: the stack is published, but a source branch moved while its local ref was being moved onto its published head, so none was moved — ccx vcs stack continue retries it: %w", stackRebasePrefix, err)
		}
		if _, err := gtRestackAlign(ctx, stackRebasePrefix, holders, moves); err != nil {
			return "", err
		}
		if err := errors.Join(gtmeta.Reparent(ctx, commonDir, reparent), gtmeta.RecordRestacked(ctx, commonDir, revisions)); err != nil {
			return "", fmt.Errorf("%s: the sources are on their published heads, but recording them in gt failed — fix the cause and run ccx vcs stack continue: %w", stackRebasePrefix, err)
		}
	}
	var segments []string
	if len(moved) > 0 {
		segments = append(segments, "moved "+strings.Join(moved, ", ")+" onto the published heads")
	}
	if len(left) > 0 {
		segments = append(segments, "left "+strings.Join(left, ", ")+" on their sources")
	}
	if len(segments) == 0 {
		return "source checkouts unchanged", nil
	}
	return strings.Join(segments, shipSep), nil
}

func stackChooseSourceMoves(ctx context.Context, l lane, run *stackRebaseRun, holders map[string]string) ([]string, error) {
	stays := map[string]bool{}
	var left []string
	for i := range run.Branches {
		b := &run.Branches[i]
		if b.Landed != "" || b.Held != "" || b.LocalOnly || b.NewHead == b.Local {
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
			if why, err = stackSourceStays(ctx, l, run, *b, at, holders, stays); err != nil {
				return nil, err
			}
		}
		if why != "" {
			stays[b.Name] = true
			left = append(left, b.Name+" ("+why+")")
			continue
		}
		b.Moved = true
	}
	return left, nil
}

func stackSourceStays(ctx context.Context, l lane, run *stackRebaseRun, b stackRebaseBranch, at string, holders map[string]string, stays map[string]bool) (string, error) {
	if stays[b.Parent] {
		return "stacked on " + b.Parent, nil
	}
	if at != b.Local {
		return "moved since the run started", nil
	}
	holder := holders[b.Name]
	if holder != "" && holder != run.Origin {
		return "checked out in " + holder, nil
	}
	source, err := stackPatchSeries(ctx, l.dir(), cmp.Or(b.SourceBase, b.OldBase), b.Local)
	if err != nil {
		return "", err
	}
	published, err := stackPatchSeries(ctx, l.dir(), b.NewBase, b.NewHead)
	if err != nil {
		return "", err
	}
	if source == nil || published == nil || !slices.Equal(source, published) {
		return "its published head carries other changes", nil
	}
	if holder == "" {
		return "", nil
	}
	status, err := render.RunCLI(ctx, render.Dir(holder), "git", []string{"status", "--porcelain", "--untracked-files=normal"})
	if err != nil {
		return "", fmt.Errorf("%s: git status in %s: %w", stackRebasePrefix, holder, err)
	}
	if status != "" {
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
	if err := stackCheckHolders(ctx, run.Origin, movers, holders); err != nil {
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
