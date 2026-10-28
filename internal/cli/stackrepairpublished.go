package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
)

func newStackRepairPublishedChildCmd() *cobra.Command {
	var parent string
	cmd := &cobra.Command{
		Use:   "repair-published-child",
		Short: "Finish metadata for a child cut from a verified published parent, or record trunk as a published branch's parent",
		Long: `Finish metadata for a child cut from a verified published parent.

--parent <trunk> records trunk as the checked-out branch's gt parent, for a
branch whose recorded parent is gone and whose open pull request GitHub
already bases on trunk. The branch's local head must be its published head.
Nothing is pushed and no commit moves: gt's parent and parent revision, the
branch's merge base with the remote trunk, are all that change.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStackRepairPublishedChild(cmd, parent)
		},
	}
	cmd.Flags().StringVar(&parent, "parent", "", "published parent branch used to create this child")
	_ = cmd.MarkFlagRequired("parent")
	return cmd
}

func runStackRepairPublishedChild(cmd *cobra.Command, parent string) error {
	ctx := cmd.Context()
	l, err := resolveWriteLane(ctx, "stack repair-published-child", workingDir(ctx), false)
	if err != nil {
		return err
	}
	if !l.gt {
		return errors.New("stack repair-published-child: requires the graphite lane")
	}
	child, err := gitCurrentBranch(ctx, l.dir(), "stack repair-published-child")
	if err != nil {
		return err
	}
	if child == "" || child == parent {
		return errors.New("stack repair-published-child: run from the published child's worktree")
	}
	common, err := gtCommonDir(ctx, l.dir(), "stack repair-published-child")
	if err != nil {
		return err
	}
	state, err := gtStateQuery(ctx, l.dir(), "stack repair-published-child")
	if err != nil {
		return err
	}
	trunk, err := gtTrunkBranch("stack repair-published-child", state)
	if err != nil {
		return err
	}
	if parent == trunk {
		return stackRepairOntoTrunk(cmd, l, common, state, child, trunk)
	}
	receipt, err := stackReadPublication(ctx, l.dir(), parent)
	if err != nil {
		return err
	}
	if receipt == nil {
		return fmt.Errorf("stack repair-published-child: %s has no publication receipt", parent)
	}
	if err := stackVerifyNewParent(ctx, l.dir(), receipt); err != nil {
		return err
	}
	submitted, err := gtmeta.LastSubmitted(ctx, common)
	if err != nil {
		return err
	}
	if last := submitted[parent]; last.HeadSha != receipt.Head || last.BaseName != receipt.Parent {
		return fmt.Errorf("stack repair-published-child: %s submission no longer matches its publication receipt", parent)
	}
	if err := stackVerifyNewRemote(ctx, l.dir(), receipt); err != nil {
		return err
	}
	if err := stackVerifyPublishedChildRefs(ctx, l.dir(), child, receipt); err != nil {
		return err
	}
	row, err := gtmeta.ReadPublishedChild(ctx, common, child)
	if err != nil {
		return err
	}
	if row.BranchRevision != receipt.Head || row.Submitted != "" || row.Children != "" && row.Children != "[]" || row.State != "" && row.State != "none" || row.ParentHeadRevision != "" {
		return fmt.Errorf("stack repair-published-child: %s metadata changed since its published-parent creation", child)
	}
	if row.Parent == parent && row.ParentRevision == receipt.Head && row.Validation == "VALID" {
		cmd.Printf("already repaired %s onto %s\n", child, parent)
		return nil
	}
	if row.Parent != "" && row.Parent != parent || row.ParentRevision != "" || row.Validation != "BAD_PARENT_NAME" {
		return fmt.Errorf("stack repair-published-child: %s has metadata outside the incomplete published-child state", child)
	}
	if err := gtmeta.Reparent(ctx, common, map[string]string{child: parent}); err != nil {
		return fmt.Errorf("stack repair-published-child: %w", err)
	}
	if err := gtmeta.RecordRestacked(ctx, common, map[string]string{child: receipt.Head}); err != nil {
		return fmt.Errorf("stack repair-published-child: %w", err)
	}
	if err := stackVerifyPublishedChildRefs(ctx, l.dir(), child, receipt); err != nil {
		return err
	}
	if err := stackVerifyNewParent(ctx, l.dir(), receipt); err != nil {
		return err
	}
	cmd.Printf("repaired %s onto published %s\n", child, parent)
	return nil
}

func stackRepairOntoTrunk(cmd *cobra.Command, l lane, common string, state gtState, child, trunk string) error {
	ctx := cmd.Context()
	const prefix = "stack repair-published-child"
	s, tracked := state[child]
	if !tracked || len(s.Parents) == 0 {
		return fmt.Errorf("%s: gt does not track %s", prefix, child)
	}
	if s.Parents[0].Ref == trunk {
		cmd.Printf("already repaired %s onto %s\n", child, trunk)
		return nil
	}
	prs, err := stackPRs(ctx, l.dir(), trunk, []string{child})
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	pr := prs[child]
	if pr == nil || pr.State != "OPEN" {
		return fmt.Errorf("%s: %s has no open pull request to read its base from", prefix, child)
	}
	if pr.base() != trunk {
		return fmt.Errorf("%s: %s's pull request %s is based on %s, not %s — retarget it first, or restack it with ccx vcs stack rebase --parent %s=%s", prefix, child, pr, pr.base(), trunk, child, trunk)
	}
	tr, err := gtTrunkRef(ctx, l.dir(), prefix, trunk)
	if err != nil {
		return err
	}
	remotes, err := stackRemoteHeads(ctx, l.dir(), prefix, tr.Remote(), []string{child}, "")
	if err != nil {
		return err
	}
	head, err := stackRevParse(ctx, l.dir(), "HEAD")
	if err != nil {
		return err
	}
	if remotes[child] != head {
		return fmt.Errorf("%s: %s's local head %.12s is not its published head %.12s — publish or reset it first", prefix, child, head, remotes[child])
	}
	base, err := stackMergeBase(ctx, l.dir(), head, string(tr.Ref()))
	if err != nil {
		return err
	}
	if err := gtmeta.Reparent(ctx, common, map[string]string{child: trunk}); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	if err := gtmeta.RecordRestacked(ctx, common, map[string]string{child: base}); err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	cmd.Printf("repaired %s onto %s at %.12s, matching %s\n", child, trunk, base, pr)
	return nil
}

func stackVerifyPublishedChildRefs(ctx context.Context, dir render.Dir, child string, receipt *stackPublication) error {
	head, err := stackRevParse(ctx, dir, "HEAD")
	if err != nil {
		return err
	}
	if head != receipt.Head {
		return fmt.Errorf("stack repair-published-child: %s HEAD changed from published %s to %s", child, receipt.Head, head)
	}
	tx := fmt.Sprintf("start\nverify %s %s\nverify %s %s\nverify %s %s\ncommit\n", gtRestackRef(child), receipt.Head, gtRestackRef(receipt.Branch), receipt.Source, stackPublicationRef(receipt.Branch, "receipt"), receipt.OID)
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(tx)); err != nil {
		return fmt.Errorf("stack repair-published-child: published refs changed during repair: %w", err)
	}
	return nil
}
