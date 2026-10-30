package cli

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/execstub"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcstest"
)

func TestFocusedStateEmptyTrunkParentKeepsRefReadsBounded(t *testing.T) {
	f := vcstest.Repo(t, vcstest.Remote())
	common := filepath.Join(f.Dir, ".git")
	head := strings.TrimSpace(f.Out(t, "git", "rev-parse", "main"))
	vcstest.WriteGraphiteMeta(t, common, fmt.Sprintf(`{"main":{"trunk":true},"requested":{"parents":[{"ref":"main","sha":%q}]},"requested-child":{"parents":[{"ref":"requested","sha":%q}]},"unrelated":{"parents":[{"ref":"main","sha":%q}]}}`, head, head, head))
	for _, branch := range []string{"requested", "requested-child", "unrelated"} {
		f.Out(t, "git", "branch", branch)
	}
	db, err := sql.Open("sqlite", filepath.Join(common, ".graphite_metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`UPDATE branch_metadata SET parent_branch_name = '' WHERE branch_name = 'main'`); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(f.ShimBin, "git-original")
	if err := os.Rename(filepath.Join(f.ShimBin, "git"), original); err != nil {
		t.Fatal(err)
	}
	input := filepath.Join(t.TempDir(), "requested-refs")
	audit := filepath.Join(t.TempDir(), "all-requested-refs")
	script := "#!/bin/sh\ncase \" $* \" in\n*for-each-ref*--stdin*)\n/bin/cat > " + shQuote(input) + "\nprintf '%s\\n' BEGIN >> " + shQuote(audit) + "\n/bin/cat " + shQuote(input) + " >> " + shQuote(audit) + "\nexec " + shQuote(original) + " \"$@\" < " + shQuote(input) + "\n;;\nesac\nexec " + shQuote(original) + " \"$@\"\n"
	execstub.Write(t, filepath.Join(f.ShimBin, "git"), script)
	state, err := gtStateAtFocused(withStackProvider(f.Context(), false), common, "test", "requested", "")
	if err != nil {
		t.Fatal(err)
	}
	if !state["main"].Trunk || state["requested"].Head != head || state["requested-child"].Head != head {
		t.Fatalf("requested topology = %+v", state)
	}
	payload, err := os.ReadFile(audit)
	if err != nil {
		t.Fatal(err)
	}
	var calls, resolved int
	for _, entry := range strings.Fields(string(payload)) {
		if entry == "BEGIN" {
			calls++
		} else {
			resolved++
		}
	}
	t.Logf("empty_trunk_parent=true ref_calls=%d resolved_refs=%d", calls, resolved)
	if calls != 1 || resolved != 4 || strings.Contains(string(payload), "unrelated") {
		t.Fatalf("resolved %d refs across %d calls with an empty trunk parent: %s", resolved, calls, payload)
	}
}

func TestFocusedStateUntrackedRequestsKeepNearestTrackedAncestor(t *testing.T) {
	for _, tc := range []struct {
		name   string
		orphan bool
	}{
		{"nonlanded orphan", true},
		{"untracked explicit parent", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := vcstest.Repo(t, vcstest.Remote())
			common := filepath.Join(f.Dir, ".git")
			main := strings.TrimSpace(f.Out(t, "git", "rev-parse", "main"))
			heads := make(map[string]string)
			for _, branch := range []string{"p", "c", "b"} {
				f.Out(t, "git", "switch", "-qc", branch)
				name := branch + ".txt"
				if err := os.WriteFile(filepath.Join(f.Dir, name), []byte(branch+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				f.Out(t, "git", "add", name)
				f.Out(t, "git", "commit", "-qm", branch)
				heads[branch] = strings.TrimSpace(f.Out(t, "git", "rev-parse", "HEAD"))
			}
			branch, parent := "b", ""
			if tc.orphan {
				vcstest.WriteGraphiteMeta(t, common, fmt.Sprintf(`{"main":{"trunk":true},"p":{"parents":[{"ref":"main","sha":%q}]},"c":{"parents":[{"ref":"main","sha":%q}]},"b":{"parents":[{"ref":"c","sha":%q}]}}`, main, main, heads["c"]))
				f.Out(t, "git", "branch", "-D", "c")
			} else {
				f.Out(t, "git", "branch", "requested", "main")
				vcstest.WriteGraphiteMeta(t, common, fmt.Sprintf(`{"main":{"trunk":true},"p":{"parents":[{"ref":"main","sha":%q}]},"requested":{"parents":[{"ref":"main","sha":%q}]}}`, main, main))
				branch, parent = "requested", "b"
			}
			ctx := withStackProvider(f.Context(), false)
			state, err := gtStateAtFocused(ctx, common, "test", branch, parent)
			if err != nil {
				t.Fatal(err)
			}
			if _, tracked := state["b"]; tracked || state["p"].Head != heads["p"] {
				t.Fatalf("untracked request state = %+v", state)
			}
			got, err := gtNearestTracked(ctx, render.Dir(f.Dir), state, "main", "b")
			if err != nil {
				t.Fatal(err)
			}
			if got != "p" {
				t.Fatalf("nearest tracked parent = %q, want p", got)
			}
		})
	}
}
