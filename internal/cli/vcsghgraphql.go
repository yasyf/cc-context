package cli

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
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

const ghRunGraphQLSelection = `databaseId runNumber url createdAt updatedAt event workflow { name databaseId } checkSuite { status conclusion commit { oid } branch { name } }`

var (
	ghRunID40   = regexp.MustCompile(`^[0-9a-f]{40}$`)
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
	if len(read.fields) == 0 || slices.ContainsFunc(read.fields, func(f string) bool { return ghRunGraphQLFields[f] == "" }) {
		return ghRunRead{}, false
	}
	if argv[1] == "view" {
		if len(positional) != 1 || !ghRunID.MatchString(positional[0]) || read.commit != "" || read.workflow != "" {
			return ghRunRead{}, false
		}
		read.run = positional[0]
		return read, true
	}
	if len(positional) != 0 || !ghRunID40.MatchString(read.commit) || strings.HasSuffix(read.workflow, ".yml") || strings.HasSuffix(read.workflow, ".yaml") || ghRunID.MatchString(read.workflow) {
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
	if r.run != "" {
		query = fmt.Sprintf(`query { repository(owner: %q, name: %q) { id } run: resource(url: %q) { ... on WorkflowRun { %s } } }`,
			owner, name, fmt.Sprintf("https://github.com/%s/actions/runs/%s", repo, r.run), ghRunGraphQLSelection)
		jq = fmt.Sprintf(`if .data.run == null or .data.run.databaseId == null then error("could not find any workflow run with ID %s") else .data.run | %s end`, r.run, shape)
	} else {
		query = fmt.Sprintf(`query { repository(owner: %q, name: %q) { object(oid: %q) { ... on Commit { checkSuites(first: 100) { nodes { workflowRun { %s } } } } } } }`,
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
