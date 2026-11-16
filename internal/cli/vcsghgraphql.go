package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/ghapi"
)

var ghRunGraphQLFields = map[string]string{
	"conclusion":         `(.checkSuite.conclusion // "" | ascii_downcase)`,
	"createdAt":          `.createdAt`,
	"databaseId":         `.databaseId`,
	"event":              `.event`,
	"headBranch":         `(.checkSuite.branch.name // "")`,
	"headSha":            `.checkSuite.commit.oid`,
	"number":             `.runNumber`,
	"status":             `(.checkSuite.status // "" | ascii_downcase)`,
	"updatedAt":          `.updatedAt`,
	"url":                `.url`,
	"workflowDatabaseId": `.workflow.databaseId`,
	"workflowName":       `.workflow.name`,
}

const (
	ghRunGraphQLSelection     = `databaseId runNumber url createdAt updatedAt event workflow { name databaseId } checkSuite { status conclusion commit { oid } branch { name } }`
	ghRunJobsGraphQLSelection = `databaseId url event workflow { name } checkSuite { status conclusion commit { oid } branch { name } checkRuns(first: 100) { nodes { databaseId name status conclusion detailsUrl } } }`
)

// ghRunTextDefs and ghRunTextShape render a run read with
// ghRunJobsGraphQLSelection as gh run view prints it: a title line, the
// trigger, one line per job, and the run's URL.
const (
	ghRunTextDefs = `def sym: if .status != "COMPLETED" then "*" elif .conclusion == "SUCCESS" then "✓" elif .conclusion == "SKIPPED" or .conclusion == "NEUTRAL" then "-" else "X" end; ` +
		`def state: (if .status == "COMPLETED" then .conclusion else .status end // "") | ascii_downcase; `
	ghRunTextShape = `(. as $r | .checkSuite as $s | [` +
		`"\($s | sym) \($s.branch.name // "") \($r.workflow.name) · \($r.databaseId)", ` +
		`"Triggered via \($r.event) · \($s | state) · \($s.commit.oid[:7])", "", "JOBS"] + ` +
		`[$s.checkRuns.nodes[] | "\(sym) \(.name) (ID \(.databaseId)): \(state)"] + ` +
		`["", "View this run on GitHub: \($r.url)"] | join("\n"))`
)

var (
	ghCommitSHA = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	ghRepoShape = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

type ghRunRead struct {
	repo, run, commit, workflow, jq string
	fields                          []string
	limit                           int
}

func ghParseRunRead(argv []string) (ghRunRead, bool) {
	if len(argv) < 2 || argv[0] != "run" || argv[1] != "view" && argv[1] != "list" {
		return ghRunRead{}, false
	}
	read := ghRunRead{limit: 20}
	var positional []string
	for i := 2; i < len(argv); i++ {
		flag, value, inline := strings.Cut(argv[i], "=")
		if !strings.HasPrefix(flag, "-") {
			positional = append(positional, argv[i])
			continue
		}
		if !inline {
			if i+1 >= len(argv) {
				return ghRunRead{}, false
			}
			i++
			value = argv[i]
		}
		switch flag {
		case "-R", "--repo":
			read.repo = value
		case "--json":
			read.fields = strings.Split(value, ",")
		case "-q", "--jq":
			read.jq = value
		case "-c", "--commit":
			read.commit = value
		case "-w", "--workflow":
			read.workflow = value
		case "-L", "--limit":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return ghRunRead{}, false
			}
			read.limit = n
		default:
			return ghRunRead{}, false
		}
	}
	text := len(read.fields) == 0 && read.jq == "" && argv[1] == "view"
	if !text && (len(read.fields) == 0 || slices.ContainsFunc(read.fields, func(f string) bool { return ghRunGraphQLFields[f] == "" })) {
		return ghRunRead{}, false
	}
	if argv[1] == "view" {
		if len(positional) != 1 || !ghRunID.MatchString(positional[0]) || read.commit != "" || read.workflow != "" {
			return ghRunRead{}, false
		}
		read.run = positional[0]
		return read, true
	}
	if len(positional) != 0 || !ghCommitSHA.MatchString(read.commit) || strings.HasSuffix(read.workflow, ".yml") || strings.HasSuffix(read.workflow, ".yaml") || ghRunID.MatchString(read.workflow) {
		return ghRunRead{}, false
	}
	return read, true
}

func (r ghRunRead) argv(repo string) []string {
	owner, name, _ := strings.Cut(repo, "/")
	pick := make([]string, 0, len(r.fields))
	for _, f := range r.fields {
		pick = append(pick, fmt.Sprintf("%s: %s", f, ghRunGraphQLFields[f]))
	}
	shape := "{" + strings.Join(pick, ", ") + "}"
	var query, jq string
	switch {
	case r.run != "" && len(r.fields) == 0:
		query = ghRunQuery(repo, r.run, ghRunJobsGraphQLSelection)
		jq = ghRunTextDefs + ghRunFound(r.run, ghRunTextShape)
	case r.run != "":
		query = ghRunQuery(repo, r.run, ghRunGraphQLSelection)
		jq = ghRunFound(r.run, shape)
	default:
		query = fmt.Sprintf(`query { repository(owner: %q, name: %q) { object(expression: %q) { ... on Commit { checkSuites(first: 100) { nodes { workflowRun { %s } } } } } } }`,
			owner, name, r.commit, ghRunGraphQLSelection)
		filter := ""
		if r.workflow != "" {
			filter = fmt.Sprintf(` | select(.workflow.name == %q)`, r.workflow)
		}
		jq = fmt.Sprintf(`[.data.repository.object.checkSuites.nodes[]?.workflowRun | select(. != null)%s] | sort_by(.databaseId) | reverse | .[:%d] | map(%s)`, filter, r.limit, shape)
	}
	if r.jq != "" {
		jq += " | " + r.jq
	}
	return []string{"api", "graphql", "-f", "query=" + query, "--jq", jq}
}

func ghRunViaGraphQL(argv []string, checkoutRepo func() string) []string {
	read, ok := ghParseRunRead(argv)
	if !ok {
		return argv
	}
	repo := read.repo
	if repo == "" {
		repo = checkoutRepo()
	}
	if !ghRepoShape.MatchString(repo) {
		return argv
	}
	return read.argv(repo)
}

func ghRunQuery(repo, run, selection string) string {
	owner, name, _ := strings.Cut(repo, "/")
	return fmt.Sprintf(`query { repository(owner: %q, name: %q) { id } run: resource(url: %q) { ... on WorkflowRun { %s } } }`,
		owner, name, fmt.Sprintf("https://github.com/%s/actions/runs/%s", repo, run), selection)
}

// ghRunFound applies shape to the run a ghRunQuery read, failing as gh does
// when the repository has no run with that ID.
func ghRunFound(run, shape string) string {
	return fmt.Sprintf(`if .data.run == null or .data.run.databaseId == null then error("could not find any workflow run with ID %s") else .data.run | %s end`, run, shape)
}

type ghRunWatch struct {
	repo, run  string
	interval   time.Duration
	exitStatus bool
}

// ghParseRunWatch reads gh run watch <id> with any of --repo, --exit-status,
// --interval, and --compact, its interval raised to ghWatchFloor.
func ghParseRunWatch(argv []string) (ghRunWatch, bool) {
	if len(argv) < 3 || argv[0] != "run" || argv[1] != "watch" {
		return ghRunWatch{}, false
	}
	watch := ghRunWatch{interval: ghWatchFloor * time.Second}
	var positional []string
	for i := 2; i < len(argv); i++ {
		flag, value, inline := ghSplitFlag(argv[i])
		switch {
		case !strings.HasPrefix(flag, "-"):
			positional = append(positional, argv[i])
			continue
		case flag == "--exit-status" && !inline:
			watch.exitStatus = true
			continue
		case flag == "--compact" && !inline:
			continue
		case !inline:
			if i+1 >= len(argv) {
				return ghRunWatch{}, false
			}
			i++
			value = argv[i]
		}
		switch flag {
		case "-R", "--repo":
			watch.repo = value
		case "-i", "--interval":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 {
				return ghRunWatch{}, false
			}
			watch.interval = time.Duration(max(n, ghWatchFloor)) * time.Second
		default:
			return ghRunWatch{}, false
		}
	}
	if len(positional) != 1 || !ghRunID.MatchString(positional[0]) {
		return ghRunWatch{}, false
	}
	watch.run = positional[0]
	return watch, true
}

func (w ghRunWatch) argv(repo string) []string {
	return []string{"api", "graphql", "-f", "query=" + ghRunQuery(repo, w.run, ghRunJobsGraphQLSelection), "--jq", ghRunFound(w.run, ".")}
}

type ghWatchedCheck struct {
	DatabaseID int64  `json:"databaseId"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type ghWatchedRun struct {
	DatabaseID int64 `json:"databaseId"`
	Workflow   struct {
		Name string `json:"name"`
	} `json:"workflow"`
	CheckSuite struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		CheckRuns  struct {
			Nodes []ghWatchedCheck `json:"nodes"`
		} `json:"checkRuns"`
	} `json:"checkSuite"`
}

func ghRunSymbol(status, conclusion string) string {
	switch {
	case status != "COMPLETED":
		return "*"
	case conclusion == "SUCCESS":
		return "✓"
	case conclusion == "SKIPPED" || conclusion == "NEUTRAL":
		return "-"
	default:
		return "X"
	}
}

func ghRunState(status, conclusion string) string {
	if status == "COMPLETED" {
		return strings.ToLower(conclusion)
	}
	return strings.ToLower(status)
}

// ghWatchRun runs gh run watch itself: the run and its jobs read in one GraphQL
// query through the gateway on the watch reserve every interval, a line printed
// each time the run or a job changes status, until the run completes.
func ghWatchRun(cmd *cobra.Command, watch ghRunWatch, repo string) error {
	ctx := ghapi.BackgroundQuota(cmd.Context())
	poll := watch.argv(repo)
	out := cmd.OutOrStdout()
	seen := map[int64]string{}
	runSeen := ""
	for {
		var answer bytes.Buffer
		gateway := &cobra.Command{}
		gateway.SetContext(ctx)
		gateway.SetIn(cmd.InOrStdin())
		gateway.SetOut(&answer)
		gateway.SetErr(cmd.ErrOrStderr())
		if err := runVcsGh(gateway, poll, ghRunStateMaxAge); err != nil {
			return err
		}
		var run ghWatchedRun
		if err := json.Unmarshal(answer.Bytes(), &run); err != nil {
			return fmt.Errorf("vcs gh: read run %s: %w", watch.run, err)
		}
		for _, check := range run.CheckSuite.CheckRuns.Nodes {
			state := ghRunState(check.Status, check.Conclusion)
			if seen[check.DatabaseID] == state {
				continue
			}
			seen[check.DatabaseID] = state
			if _, err := fmt.Fprintf(out, "%s %s (ID %d): %s\n", ghRunSymbol(check.Status, check.Conclusion), check.Name, check.DatabaseID, state); err != nil {
				return err
			}
		}
		suite := run.CheckSuite
		completed := suite.Status == "COMPLETED"
		if state := ghRunState(suite.Status, suite.Conclusion); state != runSeen {
			runSeen = state
			line := fmt.Sprintf("Run %s (%d) is %s", run.Workflow.Name, run.DatabaseID, state)
			if completed {
				line = fmt.Sprintf("Run %s (%d) completed with '%s'", run.Workflow.Name, run.DatabaseID, state)
			}
			if _, err := fmt.Fprintf(out, "%s %s\n", ghRunSymbol(suite.Status, suite.Conclusion), line); err != nil {
				return err
			}
		}
		if completed {
			if watch.exitStatus && suite.Conclusion != "SUCCESS" {
				return &ExitError{Code: 1}
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(watch.interval):
		}
	}
}
