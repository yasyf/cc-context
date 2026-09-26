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
	"github.com/yasyf/cc-context/internal/render"
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
	if b.Local == receipt.Head && b.Remote == receipt.Head {
		b.OldBase = receipt.Base
		b.SourceBase = receipt.Base
		return nil
	}
	base := receipt.Base
	if b.Remote != receipt.Head {
		replayed, err := stackReplayedOnto(ctx, dir, b.Remote, receipt.Base, receipt.Head)
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
		fmt.Fprintf(&tx, "verify %s %s\n", gtRestackRef(b.Name), b.Local)
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
		receipt := stackPublication{Branch: b.Name, Source: b.Local, SourceBase: cmp.Or(b.SourceBase, b.OldBase), Head: pushed.head, Base: pushed.baseSha, Parent: pushed.base}
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
	heads, err := stackRemoteHeads(ctx, dir, remote, names)
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
				return err
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
	if err := stackPinPublication(ctx, l.dir(), run); err != nil {
		return err
	}
	if run.deferPush {
		return nil
	}
	state, err := gtStateAt(ctx, commonDir, stackRebasePrefix)
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
		if branch.Landed == "" && branch.Held == "" {
			live = append(live, branch.Name)
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
	cmd.Printf("published %d branches · source checkouts unchanged\nproposing %d commit(s), %d file(s)\n", len(live), commits, files)
	if run.Ship != nil {
		if err := stackFinishShip(ctx, cmd, l, run, entries); err != nil {
			return err
		}
	}
	if err := stackVerdict(ctx, cmd, l.dir(), run, live); err != nil {
		return err
	}
	return stackCompletePublication(ctx, l.dir(), commonDir, run)
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
