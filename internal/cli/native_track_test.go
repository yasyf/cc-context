package cli

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/nativemeta"
	"github.com/yasyf/cc-context/internal/vcs"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestNativeTrackAdoptsWithoutGraphiteDatabase(t *testing.T) {
	t.Parallel()
	f := vcstest.Repo(t, vcstest.Remote(), vcstest.Branch("fresh"))
	writeShipFile(t, f.Dir, "fresh.txt", "native change\n")
	f.Out(t, "git", "add", "fresh.txt")
	f.Out(t, "git", "commit", "-m", "native change")
	head := strings.TrimSpace(f.Out(t, "git", "rev-parse", "fresh"))
	trunk := strings.TrimSpace(f.Out(t, "git", "rev-parse", "main"))
	common := filepath.Join(f.Dir, ".git")
	ctx := withStackProvider(f.Context(), true)
	if err := nativemeta.Init(ctx, common, "main"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".graphite_metadata.db", ".graphite_repo_config"} {
		if _, err := os.Stat(filepath.Join(common, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("fixture unexpectedly has %s: %v", name, err)
		}
	}
	checkout, err := vcs.ResolveCheckout(f.Dir)
	if err != nil {
		t.Fatal(err)
	}
	l := lane{kind: vcs.Git, root: f.Dir, checkout: checkout, gt: true, native: true}
	state, _, err := gtTrack(ctx, io.Discard, l, shipOpts{parent: "main", noCommit: true}, "fresh", newGTCache(l, "ship"))
	if err != nil {
		t.Fatal(err)
	}
	branch := state["fresh"]
	if len(branch.Parents) != 1 || branch.Parents[0].Ref != "main" || branch.Head != head {
		t.Fatalf("native branch was not adopted onto main: %+v", branch)
	}
	if native, err := nativemeta.Membership(common, "fresh"); err != nil || !native {
		t.Fatalf("native membership=%v error=%v", native, err)
	}
	if got := strings.TrimSpace(f.Out(t, "git", "rev-parse", "fresh")); got != head {
		t.Fatalf("attachment moved fresh: %s", got)
	}
	if got := strings.TrimSpace(f.Out(t, "git", "rev-parse", "main")); got != trunk {
		t.Fatalf("attachment moved main: %s", got)
	}
	if _, err := os.Stat(filepath.Join(common, ".graphite_metadata.db")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native attachment created a Graphite database: %v", err)
	}
	assertNoGT(t, vcstest.Invocations(t, f.ArgvLog))
}
