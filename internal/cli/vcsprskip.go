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
	prSkipUnproven     = "redundant skip, no full run"
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

type prSkipProof struct {
	ctx    context.Context
	client *ghapi.Client
	repo   string
}

func prProveRedundantSkips(ctx context.Context, repo string, pr prstate.PR, ci *prCIReport) {
	if ci.State != prCIGreen || pr.Rollup == nil {
		return
	}
	p := prSkipProof{ctx: ctx, client: reviewsAPI().ForRepo(repo), repo: repo}
	for _, c := range pr.Rollup.Contexts.Nodes {
		if c.Typename != "StatusContext" || c.State != "SUCCESS" || !buildkiteBuildURL.MatchString(c.TargetURL) {
			continue
		}
		skipped, err := buildkiteSkipped(ctx, c.TargetURL)
		if err != nil {
			ci.State, ci.Unproven = prCIUnproven, err.Error()
			return
		}
		if !skipped {
			continue
		}
		child, state, err := p.deferredRun(pr.HeadRefName, pr.HeadRefOid, c.Context)
		switch {
		case err != nil:
			ci.State, ci.Unproven = prCIUnproven, prSkipUnproven+": "+err.Error()
			return
		case state == prCIGreen:
			ci.Deferred = child
		case state == prCIPending:
			ci.State, ci.Running, ci.Deferred = prCIPending, ci.Running+1, child
			return
		default:
			ci.State, ci.Unproven = prCIUnproven, prSkipUnproven
			return
		}
	}
}

func (p prSkipProof) deferredRun(branch, head, statusContext string) (int, prCIState, error) {
	children, err := ghapi.Get[[]ghPull](p.ctx, p.client, fmt.Sprintf("repos/%s/pulls?base=%s&state=all&per_page=20", p.repo, url.QueryEscape(branch)))
	if err != nil {
		return 0, "", err
	}
	for _, child := range children {
		if child.State != "open" && child.MergedAt == nil {
			continue
		}
		status, err := ghapi.Get[ghCombinedStatus](p.ctx, p.client, fmt.Sprintf("repos/%s/commits/%s/status", p.repo, child.Head.SHA))
		if err != nil {
			return 0, "", err
		}
		for _, s := range status.Statuses {
			if s.Context != statusContext {
				continue
			}
			switch s.State {
			case "pending":
				return child.Number, prCIPending, nil
			case "success":
				skipped, err := buildkiteSkipped(p.ctx, s.TargetURL)
				if err != nil {
					return 0, "", err
				}
				if skipped {
					return p.deferredRun(child.Head.Ref, head, statusContext)
				}
				contains, err := p.contains(head, child.Head.SHA)
				if err != nil || contains {
					return child.Number, prCIGreen, err
				}
			}
		}
	}
	return 0, prCIUnproven, nil
}

func (p prSkipProof) contains(ancestor, head string) (bool, error) {
	compared, err := ghapi.Get[struct {
		Status string `json:"status"`
	}](p.ctx, p.client, fmt.Sprintf("repos/%s/compare/%s...%s", p.repo, ancestor, head))
	return compared.Status == "ahead" || compared.Status == "identical", err
}

func buildkiteSkipped(ctx context.Context, buildURL string) (bool, error) {
	m := buildkiteBuildURL.FindStringSubmatch(buildURL)
	if m == nil {
		return false, nil
	}
	cache, err := buildkiteVerdictPath(m[1], m[2], m[3])
	if err != nil {
		return false, err
	}
	if verdict, err := os.ReadFile(cache); err == nil { //nolint:gosec // cache is one file name under the PR state root, built from a build URL the regexp matched
		return string(verdict) == "skip", nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	token := render.Getenv(ctx, buildkiteEnv)
	if token == "" {
		return false, fmt.Errorf("no %s to read %s", buildkiteEnv, buildURL)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/organizations/%s/pipelines/%s/builds/%s", buildkiteAPI, m[1], m[2], m[3]), nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := buildkiteClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", buildURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return false, fmt.Errorf("read %s: HTTP %d: %s", buildURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var build struct {
		State string `json:"state"`
		Jobs  []struct {
			Name string `json:"name"`
		} `json:"jobs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&build); err != nil {
		return false, fmt.Errorf("read %s: %w", buildURL, err)
	}
	skipped := false
	for _, job := range build.Jobs {
		skipped = skipped || strings.Contains(strings.ToLower(job.Name), buildkiteRedundant)
	}
	if build.State == "passed" {
		verdict := map[bool]string{true: "skip", false: "full"}[skipped]
		if err := os.WriteFile(cache, []byte(verdict), 0o600); err != nil {
			return false, err
		}
	}
	return skipped, nil
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
