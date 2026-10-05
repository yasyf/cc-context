package cli

import (
	"strings"
	"testing"
)

// TestStackSubmitCorrectsAStaleGraphiteParentRecord is #30363's: Graphite
// recorded dev as its parent while the stack put it on #30342, and stack submit
// left the branch alone as unchanged, so the landing desk's enqueue refused
// #30342 as no member of the stack.
func TestStackSubmitCorrectsAStaleGraphiteParentRecord(t *testing.T) {
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	shipGTStack(t, f, "base", "feature")
	if _, _, err := runStackCmd(t, f, "submit"); err != nil {
		t.Fatalf("stack submit: %v", err)
	}
	api.prs["base"], api.prs["feature"] = 100, 101
	api.recorded["feature"] = "main"
	posted := len(api.submitHeads())

	_, errStr, err := runStackCmd(t, f, "submit")
	if err != nil {
		t.Fatalf("stack submit = %v (stderr=%q)", err, errStr)
	}
	if strings.Contains(errStr, "unchanged since its last submit: feature") || strings.Contains(errStr, "unchanged since its last submit: base, feature") {
		t.Errorf("stderr = %q, want feature resubmitted to correct its Graphite parent", errStr)
	}
	heads := api.submitHeads()[posted:]
	if len(heads) == 0 || heads[len(heads)-1] != "feature" {
		t.Fatalf("submit posts = %v, want feature resubmitted", heads)
	}
	if entry := api.submitEntry("feature"); entry.Base != "base" {
		t.Errorf("feature resubmitted onto %q, want base", entry.Base)
	}
}
