package cli

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

// shipPushAttempts bounds how many times a lane re-fetches, re-rebases, and
// re-pushes when a concurrently advanced remote rejects the push. Each retry
// does real fetch/rebase work, so there is no backoff sleep.
const shipPushAttempts = 3

// jjPushMovedSubstr is the jj wording for a bookmark the remote advanced past
// (jj 0.43: "The following references unexpectedly moved on the remote"). It is
// version-dependent; keeping it a lone constant makes a wording update a one-line
// change.
const jjPushMovedSubstr = "unexpectedly moved on the remote"

// pushRejectedError marks a push the remote refused because it advanced. Its
// invariant is re-entrancy — the next attempt can safely replay: jj earns it by
// undoing its bookmark move, git by rebases being cumulative onto successive real
// remote tips. shipPushRetry re-enters on this type alone; anything else ends the
// loop.
type pushRejectedError struct{ err error }

func (e *pushRejectedError) Error() string { return e.err.Error() }
func (e *pushRejectedError) Unwrap() error { return e.err }

// gitPushRejected reports whether err carries git's non-fast-forward rejection —
// the remote moved and our push is behind. It matches git's per-ref reason
// tokens, never the "hint:" lines advice.* config can silence, and vetoes on a
// "[remote rejected]" token: a multi-ref push can mix a non-FF ref with a
// hook-declined ref, and a hook decline is terminal.
func gitPushRejected(err error) bool {
	msg := err.Error()
	if strings.Contains(msg, "[remote rejected]") {
		return false
	}
	return strings.Contains(msg, "(non-fast-forward)") || strings.Contains(msg, "(fetch first)")
}

// gitPushStaleLease reports whether err carries git's stale-lease rejection — a
// --force-with-lease push whose remote-tracking ref no longer matches the remote.
func gitPushStaleLease(err error) bool {
	return strings.Contains(err.Error(), "(stale info)")
}

// jjPushRejected reports whether err carries jj's rejection for a bookmark the
// remote advanced past. jj parses git's porcelain records itself, so git's
// human-form "! [rejected] … (non-fast-forward)" lines never surface here. It
// must never match jj's bare "Failed to push some bookmarks" terminal line, which
// a permission failure also prints.
func jjPushRejected(err error) bool {
	return strings.Contains(err.Error(), jjPushMovedSubstr)
}

// shipPushRetry runs attempt up to shipPushAttempts times, re-entering only on a
// *pushRejectedError. It keeps the last non-zero rebase count across attempts —
// both lanes keep completed rebases, so a rebasing attempt followed by a no-rebase
// success still reports the real count. Exhaustion names the ref, the attempt
// count, and the lane's manual-recovery hint.
func shipPushRetry(ctx context.Context, ref, hint string, attempt func(context.Context) (int, error)) (int, error) {
	var (
		rebased int
		err     error
	)
	for range shipPushAttempts {
		var got int
		got, err = attempt(ctx)
		if got != 0 {
			rebased = got
		}
		var rejected *pushRejectedError
		if !errors.As(err, &rejected) {
			return rebased, err
		}
	}
	return rebased, fmt.Errorf("ship: push of %q rejected %d times — the remote refused every attempt as non-fast-forward; land manually: %s: %w", ref, shipPushAttempts, hint, err)
}

func validateShipExpectedRemote(o shipOpts, kind vcs.Kind, graphite bool) error {
	if !o.noCommit || o.noPush || kind != vcs.Git || graphite {
		return errors.New("ship: --expect-remote requires --no-commit in the plain Git lane; use --no-gt for a Git checkout configured with Graphite")
	}
	if len(o.expectRemote) != 40 && len(o.expectRemote) != 64 {
		return errors.New("ship: --expect-remote requires a full 40- or 64-character hexadecimal commit ID")
	}
	if _, err := hex.DecodeString(o.expectRemote); err != nil {
		return fmt.Errorf("ship: --expect-remote requires a hexadecimal commit ID: %w", err)
	}
	return nil
}

func shipPushGitExpected(ctx context.Context, dir render.Dir, remote, branch, expected string, noVerify bool) error {
	source, err := gtRestackHead(ctx, "ship", dir, branch)
	if err != nil {
		return err
	}
	ref := gtRestackRef(branch)
	lease := "--force-with-lease=" + ref + ":" + expected
	_, err = render.RunCLI(ctx, dir, "git", gitPushArgv(noVerify, remote, lease, source+":"+ref))
	if err != nil {
		return fmt.Errorf("ship: publish %s/%s with expected remote %s failed; the pinned lease was not refreshed — inspect and reconcile before retrying: %w", remote, branch, expected, err)
	}
	current, err := gtRestackHead(ctx, "ship", dir, branch)
	if err != nil {
		return fmt.Errorf("ship: published %s to %s/%s but could not recheck the local branch: %w", source, remote, branch, err)
	}
	if current != source {
		return fmt.Errorf("ship: published %s to %s/%s, but the local branch moved to %s during publication; left the local ref unchanged and stopped before PR and CI updates", source, remote, branch, current)
	}
	return nil
}
