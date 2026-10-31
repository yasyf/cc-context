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

const literalSearchBaselineCheckout = `      - uses: actions/checkout@v7
        with:
          ref: 32e85d05194126a4b30ed2d6703592f196d98beb
          path: .ci-literal-search-before
          persist-credentials: false
`

const semanticAncestorCandidateCheckout = `      - uses: actions/checkout@v7
        with:
          ref: ${{ inputs.commit || github.event.pull_request.head.sha || github.sha }}
`

const semanticAncestorBaselineCheckout = `      - uses: actions/checkout@v7
        with:
          ref: e39f97d38c01d78531eb8ef9c4ff71a8ae03df32
          path: .ci-semantic-ancestor-before
          persist-credentials: false
`

func checkCISourceCheckouts(ci string) error {
	job := regexp.MustCompile(`(?ms)^  coholder-path-cost:\n(.*?)(?:^  \S|\z)`).FindStringSubmatch(ci)
	if len(job) != 2 || strings.Count(job[1], coholderBaselineCheckout) != 1 || strings.Count(ci, coholderBaselineCheckout) != 1 {
		return errors.New("CI immutable coholder baseline checkout must appear exactly once in coholder-path-cost")
	}
	job = regexp.MustCompile(`(?ms)^  literal-search-scope:\n(.*?)(?:^  \S|\z)`).FindStringSubmatch(ci)
	if len(job) != 2 || strings.Count(job[1], literalSearchBaselineCheckout) != 1 || strings.Count(ci, literalSearchBaselineCheckout) != 1 {
		return errors.New("CI immutable literal-search baseline checkout must appear exactly once in literal-search-scope")
	}
	job = regexp.MustCompile(`(?ms)^  semantic-ancestor-scope:\n(.*?)(?:^  \S|\z)`).FindStringSubmatch(ci)
	if len(job) != 2 || strings.Count(job[1], semanticAncestorBaselineCheckout) != 1 || strings.Count(ci, semanticAncestorBaselineCheckout) != 1 {
		return errors.New("CI immutable semantic ancestor baseline checkout must appear exactly once in semantic-ancestor-scope")
	}
	if strings.Count(job[1], semanticAncestorCandidateCheckout) != 1 || strings.Count(ci, semanticAncestorCandidateCheckout) != 1 {
		return errors.New("CI semantic ancestor candidate checkout must appear exactly once in semantic-ancestor-scope")
	}
	candidates := strings.Replace(ci, coholderBaselineCheckout, "", 1)
	candidates = strings.Replace(candidates, literalSearchBaselineCheckout, "", 1)
	candidates = strings.Replace(candidates, semanticAncestorBaselineCheckout, "", 1)
	candidates = strings.Replace(candidates, semanticAncestorCandidateCheckout, "", 1)
	checkouts := strings.Count(candidates, "uses: actions/checkout@v7")
	refs := strings.Count(candidates, "ref: ${{ inputs.commit || github.sha }}")
	if checkouts == 0 || checkouts != refs {
		return fmt.Errorf("CI candidate exact checkout refs = %d, want %d nonzero checkouts", refs, checkouts)
	}
	return nil
}

func TestCISourceCheckoutContract(t *testing.T) {
	candidate := "      - uses: actions/checkout@v7\n        with:\n          ref: ${{ inputs.commit || github.sha }}\n"
	workflow := "jobs:\n  coholder-path-cost:\n" + coholderBaselineCheckout + "  literal-search-scope:\n" + literalSearchBaselineCheckout + "  semantic-ancestor-scope:\n" + semanticAncestorCandidateCheckout + semanticAncestorBaselineCheckout + "  release:\n" + candidate
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
		{"missing literal baseline", strings.Replace(workflow, literalSearchBaselineCheckout, "", 1), "immutable literal-search baseline"},
		{"mutable literal baseline", strings.Replace(workflow, "ref: 32e85d05194126a4b30ed2d6703592f196d98beb", "ref: main", 1), "immutable literal-search baseline"},
		{"literal baseline credentials", strings.Replace(workflow, literalSearchBaselineCheckout, strings.Replace(literalSearchBaselineCheckout, "persist-credentials: false", "persist-credentials: true", 1), 1), "immutable literal-search baseline"},
		{"literal baseline wrong job", strings.Replace(workflow, "literal-search-scope:", "another-job:", 1), "immutable literal-search baseline"},
		{"literal baseline wrong path", strings.Replace(workflow, "path: .ci-literal-search-before", "path: .ci-other-before", 1), "immutable literal-search baseline"},
		{"duplicate literal baseline", strings.Replace(workflow, literalSearchBaselineCheckout, literalSearchBaselineCheckout+literalSearchBaselineCheckout, 1), "immutable literal-search baseline"},
		{"missing semantic ancestor baseline", strings.Replace(workflow, semanticAncestorBaselineCheckout, "", 1), "immutable semantic ancestor baseline"},
		{"mutable semantic ancestor baseline", strings.Replace(workflow, "ref: e39f97d38c01d78531eb8ef9c4ff71a8ae03df32", "ref: main", 1), "immutable semantic ancestor baseline"},
		{"semantic ancestor baseline credentials", strings.Replace(workflow, semanticAncestorBaselineCheckout, strings.Replace(semanticAncestorBaselineCheckout, "persist-credentials: false", "persist-credentials: true", 1), 1), "immutable semantic ancestor baseline"},
		{"semantic ancestor baseline wrong job", strings.Replace(workflow, "semantic-ancestor-scope:", "another-job:", 1), "immutable semantic ancestor baseline"},
		{"semantic ancestor baseline wrong path", strings.Replace(workflow, "path: .ci-semantic-ancestor-before", "path: .ci-other-before", 1), "immutable semantic ancestor baseline"},
		{"duplicate semantic ancestor baseline", strings.Replace(workflow, semanticAncestorBaselineCheckout, semanticAncestorBaselineCheckout+semanticAncestorBaselineCheckout, 1), "immutable semantic ancestor baseline"},
		{"missing semantic ancestor candidate", strings.Replace(workflow, semanticAncestorCandidateCheckout, "", 1), "semantic ancestor candidate"},
		{"mutable semantic ancestor candidate", strings.Replace(workflow, semanticAncestorCandidateCheckout, strings.Replace(semanticAncestorCandidateCheckout, "ref: ${{ inputs.commit || github.event.pull_request.head.sha || github.sha }}", "ref: main", 1), 1), "semantic ancestor candidate"},
		{"semantic ancestor candidate loses explicit commit", strings.Replace(workflow, "inputs.commit || github.event.pull_request.head.sha || github.sha", "github.event.pull_request.head.sha || github.sha", 1), "semantic ancestor candidate"},
		{"semantic ancestor candidate wrong job", strings.Replace(workflow, semanticAncestorCandidateCheckout, "", 1) + semanticAncestorCandidateCheckout, "semantic ancestor candidate"},
		{"duplicate semantic ancestor candidate", strings.Replace(workflow, semanticAncestorCandidateCheckout, semanticAncestorCandidateCheckout+semanticAncestorCandidateCheckout, 1), "semantic ancestor candidate"},
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
