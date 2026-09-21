package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/yasyf/cc-context/internal/ghapi"
)

// statusNamedAlerts caps how many alerts the report names.
const statusNamedAlerts = 5

// statusAlert is one open code-scanning alert on the head.
type statusAlert struct {
	Rule string `json:"rule"`
	Path string `json:"path"`
}

// statusAlerts is what code scanning says about the head, split by whether the
// diff is responsible. The split is the whole point: a repository carrying a
// backlog reports alerts on every pull request opened against it, and a count
// that does not separate the two is noise on all of them.
//
// Unreadable is not the same as none. A viewer without the scope is refused,
// and reporting that refusal as a clean head is a false green with the same
// shape as every other one this report exists to catch.
type statusAlerts struct {
	Introduced []statusAlert `json:"introduced,omitempty"`
	Inherited  int           `json:"inherited,omitempty"`
	Partial    bool          `json:"partial,omitempty"`
	Unreadable string        `json:"unreadable,omitempty"`
}

// statusAlertNode is one alert as the REST list answers it.
type statusAlertNode struct {
	Rule struct {
		ID string `json:"id"`
	} `json:"rule"`
	MostRecentInstance struct {
		Location struct {
			Path string `json:"path"`
		} `json:"location"`
	} `json:"most_recent_instance"`
}

// statusAlertPath names the endpoint listing the open alerts on one pull
// request head. It asks by ref rather than by pull request, because the alerts
// belong to the commit and a pull request has no alert resource of its own.
func statusAlertPath(owner, repo string, number int) string {
	return fmt.Sprintf("/repos/%s/%s/code-scanning/alerts?ref=refs/pull/%d/head&state=open&per_page=100",
		owner, repo, number)
}

// statusReadAlerts lists the open code-scanning alerts on one pull request
// head. A repository that has never run an analysis answers 404, which is an
// honest nothing; a viewer without the scope is refused, which is not, and
// comes back as a reason rather than an empty list.
func statusReadAlerts(ctx context.Context, api *ghapi.Client, owner, repo string, number int) ([]statusAlert, string) {
	nodes, err := ghapi.Paginate[statusAlertNode](ctx, api, statusAlertPath(owner, repo, number))
	if err != nil {
		return nil, statusAlertRefusal(err)
	}
	alerts := make([]statusAlert, 0, len(nodes))
	for _, n := range nodes {
		alerts = append(alerts, statusAlert{Rule: n.Rule.ID, Path: n.MostRecentInstance.Location.Path})
	}
	return alerts, ""
}

// statusAlertRefusal reads why code scanning would not answer. A repository
// with no analysis is not a refusal and reports none at all.
func statusAlertRefusal(err error) string {
	if errors.Is(err, ghapi.ErrNotFound) {
		return ""
	}
	var status *ghapi.StatusError
	if errors.As(err, &status) && (status.Status == http.StatusForbidden || status.Status == http.StatusUnauthorized) {
		return "code scanning refused this viewer (" + http.StatusText(status.Status) +
			"), so nothing here says whether any alert is open"
	}
	return "code scanning could not be read (" + prOneLineAlert(err.Error()) +
		"), so nothing here says whether any alert is open"
}

// prOneLineAlert folds a multi-line failure onto the single line a report value
// occupies.
func prOneLineAlert(s string) string { return strings.Join(strings.Fields(s), " ") }

// statusSplitAlerts separates the alerts this diff is responsible for from the
// ones the base already carried. Partial records that the changed-file list was
// truncated, in which case an alert on a file past the cap reads as inherited
// when the diff may well have introduced it.
func statusSplitAlerts(alerts []statusAlert, changed []string, truncated bool) statusAlerts {
	in := make(map[string]bool, len(changed))
	for _, path := range changed {
		in[path] = true
	}
	var out statusAlerts
	out.Partial = truncated
	for _, alert := range alerts {
		if in[alert.Path] {
			out.Introduced = append(out.Introduced, alert)
			continue
		}
		out.Inherited++
	}
	return out
}

// statusAlertsValue renders one alerts line: the ones this diff is responsible
// for by name and capped, the rest as a count.
func statusAlertsValue(a statusAlerts) string {
	if a.Unreadable != "" {
		return a.Unreadable
	}
	if len(a.Introduced) == 0 {
		if a.Inherited == 0 {
			return ""
		}
		return fmt.Sprintf("%d open on the base already, none on a file this diff touches", a.Inherited)
	}
	named, rest := a.Introduced, ""
	if len(named) > statusNamedAlerts {
		named, rest = named[:statusNamedAlerts], fmt.Sprintf("%s+%d more", shipSep, len(a.Introduced)-statusNamedAlerts)
	}
	segs := make([]string, 0, len(named))
	for _, alert := range named {
		segs = append(segs, alert.Rule+" "+alert.Path)
	}
	line := strings.Join(segs, shipSep) + rest + shipSep +
		fmt.Sprintf("on files this diff touches; %d inherited", a.Inherited)
	if a.Partial {
		line += "; the changed-file list was truncated, so an alert past the cap reads as inherited"
	}
	return line
}

// statusAlertBlockers names the alerts this diff is answerable for. An
// inherited one is not this pull request problem and is left off the blockers.
func statusAlertBlockers(a statusAlerts) []string {
	if a.Unreadable != "" {
		return []string{a.Unreadable}
	}
	if len(a.Introduced) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%d code-scanning %s open on %s this diff touches, starting with %s in %s",
		len(a.Introduced), plural(len(a.Introduced), "alert", "alerts"),
		plural(len(a.Introduced), "a file", "files"),
		a.Introduced[0].Rule, a.Introduced[0].Path)}
}

// statusFillAlerts reads the code-scanning alerts open on each open pull
// request head and splits them by whether this diff touches the file. Only the
// open ones: an alert a closed pull request never fixed is the base problem now.
//
// One request per pull request, because alerts are listed per ref and there is
// no batched form. A repository with no analysis costs one 404 and says nothing.
func statusFillAlerts(ctx context.Context, st *vcsStatus, nodes []*statusPRNode) {
	owner, repo, ok := strings.Cut(st.Repo, "/")
	if !ok {
		return
	}
	var api *ghapi.Client
	for i, node := range nodes {
		if node == nil || node.State != "OPEN" || st.Branches[i].PR == nil {
			continue
		}
		if api == nil {
			api = reviewsAPI()
		}
		alerts, refused := statusReadAlerts(ctx, api, owner, repo, node.Number)
		if refused != "" {
			st.Branches[i].PR.Alerts = statusAlerts{Unreadable: refused}
			continue
		}
		changed := make([]string, 0, len(node.Files.Nodes))
		for _, f := range node.Files.Nodes {
			changed = append(changed, f.Path)
		}
		st.Branches[i].PR.Alerts = statusSplitAlerts(alerts, changed, node.Files.TotalCount > len(changed))
	}
}
