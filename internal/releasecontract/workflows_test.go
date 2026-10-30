package releasecontract

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPostreleaseWorkflows(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	read := func(name string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	ci := read("ci.yml")
	guides := read("guides.yml")
	release := read("release.yml")
	for name, workflow := range map[string]string{"CI": ci, "Guides": guides} {
		for _, required := range []string{
			"commit:",
			"required: true",
			"${{ inputs.commit || github.sha }}",
		} {
			if !strings.Contains(workflow, required) {
				t.Errorf("%s workflow missing %q", name, required)
			}
		}
	}
	if full, ubuntu := strings.Count(ci, `'["ubuntu-latest", "macos-latest"]'`), strings.Count(ci, "(github.event_name == 'pull_request' || inputs.commit != '') && '[\"ubuntu-latest\"]'"); full != ubuntu {
		t.Errorf("CI runs %d of its %d macOS matrices on explicit-commit validation; want none", full-ubuntu, full)
	}
	if checkouts, refs := strings.Count(ci, "uses: actions/checkout@v7"), strings.Count(ci, "ref: ${{ inputs.commit || github.sha }}"); checkouts != refs {
		t.Errorf("CI exact checkout refs = %d, want %d", refs, checkouts)
	}
	for _, required := range []string{
		`echo "sha=$(git rev-parse HEAD)" >> "$GITHUB_OUTPUT"`,
		"commit: ${{ steps.commit.outputs.sha }}",
		"post-release-ci:",
		"post-release-guides:",
		"uses: ./.github/workflows/ci.yml",
		"parallel_commits: true",
	} {
		if !strings.Contains(release, required) {
			t.Errorf("Release workflow missing %q", required)
		}
	}
	if got := strings.Count(release, "needs: descriptor"); got != 2 {
		t.Errorf("native descriptor dependencies = %d, want 2", got)
	}
	if got := strings.Count(release, "commit: ${{ needs.descriptor.outputs.commit }}"); got != 2 {
		t.Errorf("native exact-commit inputs = %d, want 2", got)
	}
	for _, forbidden := range []string{"actions: write", "gh run watch", "/dispatches"} {
		if strings.Contains(release, forbidden) {
			t.Errorf("Release workflow retains %q", forbidden)
		}
	}
}
