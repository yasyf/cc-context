package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestShipAmendOfAQueuedBranchRefusesInsteadOfClaimingItPublished is
// breakglass-land's #26045: ship --amend of a queued branch kept it at its
// published head to spare the queue, then reported "published" that old head.
func TestShipAmendOfAQueuedBranchRefusesInsteadOfClaimingItPublished(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	api := stubGTAPI(t)
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["feature"] = 100
	api.queued["feature"] = true
	stubStackPRs(t, map[string]*stackPR{"feature": {Number: 100, Title: "feature", State: "OPEN", Base: "main"}})
	queued := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature")
	writeShipFile(t, f.Dir, "feature.txt", "amended\n")

	out, errStr, err := runShipCmdFull(f.Context(), t, "--amend", "--no-watch", "feature.txt")
	if err == nil {
		t.Fatalf("ship --amend of a queued branch = %q, want a refusal", out)
	}
	for _, want := range []string{"feature is in the merge queue", "the amend stays committed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v (stderr=%q), want %q", err, errStr, want)
		}
	}
	if got := gitAt(t, f.Env(), f.RemoteDir, "rev-parse", "feature"); got != queued {
		t.Errorf("origin feature moved to %s while queued at %s", got, queued)
	}
}

func TestShipAmendNamesTheRemotesRefusalAndTheResume(t *testing.T) {
	f := stackRebaseRepo(t, "feature")
	stubGTAPI(t)
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	stubStackPRs(t, map[string]*stackPR{"feature": {Number: 100, Title: "feature", State: "OPEN", Base: "main"}})
	stackAdvanceTrunk(t, f, "upstream.txt", "upstream\n")
	writeShipExecutable(t, filepath.Join(f.RemoteDir, "hooks"), "pre-receive", "#!/bin/sh\nexit 1\n")
	writeShipFile(t, f.Dir, "feature.txt", "amended\n")

	_, _, err := runShipCmdFull(f.Context(), t, "--amend", "--no-watch", "feature.txt")
	if err == nil {
		t.Fatal("ship --amend pushed past a declining remote")
	}
	for _, want := range []string{
		"ship: the atomic push of feature moved nothing: ! [remote rejected] ",
		"-> feature (pre-receive hook declined). The commit already landed",
		"ccx vcs ship --no-commit",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q, want it to carry %q", err, want)
		}
	}
}
