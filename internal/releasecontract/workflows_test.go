package releasecontract

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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
	if err := checkCISourceCheckouts(ci); err != nil {
		t.Error(err)
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

const coholderBaselineCheckout = `      - uses: actions/checkout@v7
        with:
          ref: 59bbfcbd3cf30240f41f4b360638b13a92ef8810
          path: .ci-coholder-baseline
          persist-credentials: false
`

func checkCISourceCheckouts(ci string) error {
	job := regexp.MustCompile(`(?ms)^  coholder-path-cost:\n(.*?)(?:^  \S|\z)`).FindStringSubmatch(ci)
	if len(job) != 2 || strings.Count(job[1], coholderBaselineCheckout) != 1 || strings.Count(ci, coholderBaselineCheckout) != 1 {
		return errors.New("CI immutable coholder baseline checkout must appear exactly once in coholder-path-cost")
	}
	candidates := strings.Replace(ci, coholderBaselineCheckout, "", 1)
	checkouts := strings.Count(candidates, "uses: actions/checkout@v7")
	refs := strings.Count(candidates, "ref: ${{ inputs.commit || github.sha }}")
	if checkouts == 0 || checkouts != refs {
		return fmt.Errorf("CI candidate exact checkout refs = %d, want %d nonzero checkouts", refs, checkouts)
	}
	return nil
}

func TestCISourceCheckoutContract(t *testing.T) {
	candidate := "      - uses: actions/checkout@v7\n        with:\n          ref: ${{ inputs.commit || github.sha }}\n"
	workflow := "jobs:\n  coholder-path-cost:\n" + coholderBaselineCheckout + "  release:\n" + candidate
	for _, tc := range []struct {
		name string
		ci   string
		want string
	}{
		{"exact candidate and immutable baseline", workflow, ""},
		{"missing baseline", strings.Replace(workflow, coholderBaselineCheckout, "", 1), "immutable coholder baseline"},
		{"mutable baseline", strings.Replace(workflow, "ref: 59bbfcbd3cf30240f41f4b360638b13a92ef8810", "ref: main", 1), "immutable coholder baseline"},
		{"baseline credentials", strings.Replace(workflow, "persist-credentials: false", "persist-credentials: true", 1), "immutable coholder baseline"},
		{"baseline wrong job", strings.Replace(workflow, "coholder-path-cost:", "another-job:", 1), "immutable coholder baseline"},
		{"duplicate baseline", strings.Replace(workflow, coholderBaselineCheckout, coholderBaselineCheckout+coholderBaselineCheckout, 1), "immutable coholder baseline"},
		{"mutable candidate", strings.Replace(workflow, "ref: ${{ inputs.commit || github.sha }}", "ref: main", 1), "candidate exact checkout"},
		{"missing candidate", strings.Replace(workflow, candidate, "", 1), "candidate exact checkout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkCISourceCheckouts(tc.ci)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("checkout contract error = %v, want %q", err, tc.want)
			}
		})
	}
}
