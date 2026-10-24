package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/prstate"
	"github.com/yasyf/cc-context/internal/render"
)

const (
	buildkiteEnv       = "BUILDKITE_API_TOKEN"
	buildkiteRedundant = "redundant within its stack"
)

var (
	buildkiteAPI      = "https://api.buildkite.com/v2"
	buildkiteClient   = &http.Client{Timeout: 20 * time.Second}
	buildkiteBuildURL = regexp.MustCompile(`^https://buildkite\.com/([^/]+)/([^/]+)/builds/(\d+)$`)
)

type ghCombinedStatus struct {
	Statuses []struct {
		Context   string `json:"context"`
		State     string `json:"state"`
		TargetURL string `json:"target_url"`
	} `json:"statuses"`
}

type buildkiteRun string

const (
	buildkiteSkip       buildkiteRun = "skip"
	buildkiteFull       buildkiteRun = "full"
	buildkiteUnfinished buildkiteRun = "unfinished"
)

type prSkipProof struct {
	ctx    context.Context
	client *ghapi.Client
	repo   string
}

func prProveRedundantSkips(ctx context.Context, warn io.Writer, repo string, pr prstate.PR, ci *prCIReport) {
	if ci.State != prCIGreen || pr.Rollup == nil {
		return
	}
	p := prSkipProof{ctx: ctx, client: reviewsAPI().ForRepo(repo), repo: repo}
	for _, c := range pr.Rollup.Contexts.Nodes {
		if c.Typename != "StatusContext" || c.State != "SUCCESS" || !buildkiteBuildURL.MatchString(c.TargetURL) {
			continue
		}
		run, err := buildkiteRead(ctx, c.TargetURL)
		if err != nil {
			_, _ = fmt.Fprintf(warn, "pr status: #%d: %v; kept GitHub's verdict\n", pr.Number, err)
			return
		}
		switch run {
		case buildkiteFull:
			continue
		case buildkiteUnfinished:
			ci.State, ci.Running = prCIPending, ci.Running+1
			return
		}
		child, state, err := p.deferredRun(pr.HeadRefName, pr.HeadRefOid, c.Context)
		switch {
		case err != nil:
			_, _ = fmt.Fprintf(warn, "pr status: #%d: %s is a redundant skip, and the run it deferred to could not be read: %v; kept GitHub's verdict\n", pr.Number, c.TargetURL, err)
			return
		case state == prCIGreen:
			ci.Deferred = child
		case state == prCIPending:
			ci.State, ci.Running, ci.Deferred = prCIPending, ci.Running+1, child
			return
		default:
			ci.State = prCIUnproven
			return
		}
	}
}

func (p prSkipProof) deferredRun(branch, head, statusContext string) (int, prCIState, error) {
	children, err := ghapi.Paginate[ghPull](p.ctx, p.client, fmt.Sprintf("repos/%s/pulls?base=%s&state=all&per_page=100", p.repo, url.QueryEscape(branch)))
	if err != nil {
		return 0, "", err
	}
	pending := 0
	for _, child := range children {
		if child.State != "open" && child.MergedAt == nil {
			continue
		}
		number, state, err := p.childRun(child, head, statusContext)
		if err != nil || state == prCIGreen {
			return number, state, err
		}
		if state == prCIPending && pending == 0 {
			pending = number
		}
	}
	if pending != 0 {
		return pending, prCIPending, nil
	}
	return 0, prCIUnproven, nil
}

func (p prSkipProof) childRun(child ghPull, head, statusContext string) (int, prCIState, error) {
	status, err := ghapi.Get[ghCombinedStatus](p.ctx, p.client, fmt.Sprintf("repos/%s/commits/%s/status", p.repo, child.Head.SHA))
	if err != nil {
		return 0, "", err
	}
	for _, s := range status.Statuses {
		if s.Context != statusContext {
			continue
		}
		switch {
		case s.State == "pending":
			return child.Number, prCIPending, nil
		case s.State != "success" || !buildkiteBuildURL.MatchString(s.TargetURL):
			return 0, prCIUnproven, nil
		}
		run, err := buildkiteRead(p.ctx, s.TargetURL)
		if err != nil {
			return 0, "", err
		}
		switch run {
		case buildkiteSkip:
			return p.deferredRun(child.Head.Ref, head, statusContext)
		case buildkiteUnfinished:
			return child.Number, prCIPending, nil
		}
		contains, err := p.contains(head, child.Head.SHA)
		if err != nil || !contains {
			return 0, prCIUnproven, err
		}
		return child.Number, prCIGreen, nil
	}
	return 0, prCIUnproven, nil
}

func (p prSkipProof) contains(ancestor, head string) (bool, error) {
	compared, err := ghapi.Get[struct {
		Status string `json:"status"`
	}](p.ctx, p.client, fmt.Sprintf("repos/%s/compare/%s...%s", p.repo, ancestor, head))
	return compared.Status == "ahead" || compared.Status == "identical", err
}

func buildkiteRead(ctx context.Context, buildURL string) (buildkiteRun, error) {
	m := buildkiteBuildURL.FindStringSubmatch(buildURL)
	cache, err := buildkiteVerdictPath(m[1], m[2], m[3])
	if err != nil {
		return "", err
	}
	cached, err := os.ReadFile(cache) //nolint:gosec // cache is one file name under the PR state root, built from a build URL the regexp matched
	switch run := buildkiteRun(cached); {
	case err == nil && (run == buildkiteSkip || run == buildkiteFull):
		return run, nil
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return "", err
	}
	token := render.Getenv(ctx, buildkiteEnv)
	if token == "" {
		return "", fmt.Errorf("no %s to read %s", buildkiteEnv, buildURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/organizations/%s/pipelines/%s/builds/%s", buildkiteAPI, m[1], m[2], m[3]), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := buildkiteClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", buildURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("read %s: HTTP %d: %s", buildURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var build struct {
		State string `json:"state"`
		Jobs  []struct {
			Name string `json:"name"`
		} `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&build); err != nil {
		return "", fmt.Errorf("read %s: %w", buildURL, err)
	}
	if build.State != "passed" {
		return buildkiteUnfinished, nil
	}
	run := buildkiteFull
	for _, job := range build.Jobs {
		if strings.Contains(strings.ToLower(job.Name), buildkiteRedundant) {
			run = buildkiteSkip
		}
	}
	return run, buildkiteCache(cache, run)
}

func buildkiteCache(path string, run buildkiteRun) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.WriteString(string(run)); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func buildkiteVerdictPath(org, pipeline, number string) (string, error) {
	root, err := prStateRoot()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "buildkite")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, org+"-"+pipeline+"-"+number), nil
}
