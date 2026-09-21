package cli

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/ghapi"
)

// TestStatusSplitAlerts pins the split that makes an alert count worth reading.
// A repository carrying a backlog puts alerts on every pull request opened
// against it, so the ones this diff is answerable for are the only actionable
// half.
func TestStatusSplitAlerts(t *testing.T) {
	t.Parallel()
	alerts := []statusAlert{
		{Rule: "js/path-injection", Path: "scripts/smoke.mjs"},
		{Rule: "js/clear-text-cookie", Path: "dashboard/src/cookie.ts"},
		{Rule: "js/missing-rate-limiting", Path: "api/src/routes/release.ts"},
	}
	got := statusSplitAlerts(alerts, []string{"dashboard/src/cookie.ts", "README.md"}, false)
	if len(got.Introduced) != 1 || got.Introduced[0].Path != "dashboard/src/cookie.ts" {
		t.Errorf("introduced = %+v, want only the alert on a file the diff touches", got.Introduced)
	}
	if got.Inherited != 2 {
		t.Errorf("inherited = %d, want 2", got.Inherited)
	}
	if got.Partial {
		t.Error("partial = true with a complete changed-file list")
	}
	clean := statusSplitAlerts(nil, []string{"a.go"}, false)
	if statusAlertsValue(clean) != "" {
		t.Errorf("alerts line = %q, want silence when code scanning found nothing", statusAlertsValue(clean))
	}
	if statusAlertBlockers(clean) != nil {
		t.Error("a clean head produced a blocker")
	}
}

// TestStatusSplitAlertsSaysWhenTheDiffWasTruncated pins the bound on the split.
// The changed-file list is capped, and an alert on a file past the cap reads as
// inherited when the diff may well have introduced it — which the line has to
// admit rather than present a partial split as a whole one.
func TestStatusSplitAlertsSaysWhenTheDiffWasTruncated(t *testing.T) {
	t.Parallel()
	got := statusSplitAlerts(
		[]statusAlert{{Rule: "js/path-injection", Path: "a.mjs"}, {Rule: "js/xss", Path: "past-the-cap.ts"}},
		[]string{"a.mjs"}, true)
	if !got.Partial {
		t.Fatal("partial = false with a truncated changed-file list")
	}
	line := statusAlertsValue(got)
	if !strings.Contains(line, "truncated") {
		t.Errorf("alerts line = %q, want it to admit the split is partial", line)
	}
}

// TestStatusAlertRefusalSeparatesNoneFromNotAllowed is the reading this whole
// lane exists to get right. A repository that has never run an analysis answers
// 404 and genuinely has nothing to report; a viewer without the scope is
// refused, and reporting that as a clean head is a false green with a different
// cause.
func TestStatusAlertRefusalSeparatesNoneFromNotAllowed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		err   error
		want  string
		clean bool
	}{
		{
			name:  "a repository with no analysis has nothing to report",
			err:   &ghapi.StatusError{Status: http.StatusNotFound, Message: "no analysis found"},
			clean: true,
		},
		{
			name: "a refused viewer is not a clean head",
			err:  &ghapi.StatusError{Status: http.StatusForbidden, Message: "Resource not accessible"},
			want: "refused this viewer",
		},
		{
			name: "an unauthorized viewer is not a clean head either",
			err:  &ghapi.StatusError{Status: http.StatusUnauthorized},
			want: "refused this viewer",
		},
		{
			name: "any other failure is not a clean head",
			err:  errors.New("dial tcp: lookup api.github.com: no such host"),
			want: "could not be read",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := statusAlertRefusal(tt.err)
			if tt.clean {
				if got != "" {
					t.Errorf("statusAlertRefusal = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("statusAlertRefusal = %q, want it to contain %q", got, tt.want)
			}
			if !strings.Contains(got, "nothing here says whether any alert is open") {
				t.Errorf("statusAlertRefusal = %q, want it to refuse to imply a clean head", got)
			}
			blockers := statusAlertBlockers(statusAlerts{Unreadable: got})
			if len(blockers) != 1 {
				t.Errorf("blockers = %q, want the unreadable state surfaced as one", blockers)
			}
		})
	}
}

// TestStatusAlertPathAsksByHeadRef pins that the alerts asked for are the ones
// on this head. Code scanning has no per-pull-request resource, so asking for
// the wrong ref answers about the wrong commit.
func TestStatusAlertPathAsksByHeadRef(t *testing.T) {
	t.Parallel()
	got := statusAlertPath("Forge-AI", "monorepo", 23633)
	for _, want := range []string{"/repos/Forge-AI/monorepo/code-scanning/alerts", "ref=refs/pull/23633/head", "state=open"} {
		if !strings.Contains(got, want) {
			t.Errorf("statusAlertPath = %q, want it to contain %q", got, want)
		}
	}
}
