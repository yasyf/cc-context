package prstate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/yasyf/cc-context/internal/ghapi"
)

const (
	restDeadline = 20 * time.Second
	restWorkers  = 8
)

var errNoPull = errors.New("no such pull request")

type restPull struct {
	Number         int       `json:"number"`
	State          string    `json:"state"`
	Merged         bool      `json:"merged"`
	Title          string    `json:"title"`
	CreatedAt      time.Time `json:"created_at"`
	Draft          bool      `json:"draft"`
	ChangedFiles   int       `json:"changed_files"`
	Mergeable      *bool     `json:"mergeable"`
	MergeableState string    `json:"mergeable_state"`
	MergeCommitSHA string    `json:"merge_commit_sha"`
	User           *struct {
		Login string `json:"login"`
	} `json:"user"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

type restReview struct {
	State string `json:"state"`
	User  *struct {
		Login string `json:"login"`
	} `json:"user"`
}

type restCheckRuns struct {
	CheckRuns []struct {
		Name       string `json:"name"`
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	} `json:"check_runs"`
}

type restStatus struct {
	State      string `json:"state"`
	TotalCount int    `json:"total_count"`
	Statuses   []struct {
		Context string `json:"context"`
		State   string `json:"state"`
	} `json:"statuses"`
}

type restComment struct {
	Body string `json:"body"`
}

type restCompare struct {
	Status string `json:"status"`
}

func (g *GitHub) pollREST(ctx context.Context, numbers []int, prev map[int]PR, trunk string) (poll, error) {
	ctx, cancel := context.WithTimeout(ctx, restDeadline)
	defer cancel()
	infos, mergeability := g.queueRecords(ctx, numbers)
	p := pass{GitHub: g, prev: prev, infos: infos, mergeability: mergeability, out: poll{prs: make(map[int]PR, len(numbers)), trunk: Trunk{Name: trunk}}}
	var mu sync.Mutex
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(restWorkers)
	for _, n := range numbers {
		t := p.target(n)
		group.Go(func() error {
			pr, err := p.restRecord(ctx, t)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case errors.Is(err, errNoPull):
				p.out.missing = append(p.out.missing, n)
			case err != nil:
				return fmt.Errorf("prstate: rest #%d: %w", n, err)
			default:
				p.out.prs[n] = pr
			}
			return nil
		})
	}
	return p.out, group.Wait()
}

func (p *pass) restRecord(ctx context.Context, t target) (PR, error) {
	repo := "repos/" + p.owner + "/" + p.name
	pull, err := ghapi.Get[restPull](ctx, p.gh, fmt.Sprintf("%s/pulls/%d", repo, t.number))
	if errors.Is(err, ghapi.ErrNotFound) {
		return PR{}, errNoPull
	}
	if err != nil {
		return PR{}, err
	}
	pr := PR{
		Number:           pull.Number,
		State:            restState(pull),
		Title:            pull.Title,
		CreatedAt:        pull.CreatedAt,
		BaseRefName:      pull.Base.Ref,
		HeadRefName:      pull.Head.Ref,
		HeadRefOid:       pull.Head.SHA,
		Mergeable:        restMergeable(pull.Mergeable),
		MergeStateStatus: strings.ToUpper(pull.MergeableState),
		Draft:            pull.Draft,
		ChangedFiles:     pull.ChangedFiles,
		Activity:         p.prev[t.number].Activity,
		Graphite:         p.infos[t.number],
		Mergeability:     p.mergeability[t.number],
		QueueUnknown:     p.queueUnknown(),
		Source:           SourceREST,
	}
	if pull.User != nil {
		pr.Author = pull.User.Login
	}
	if pull.Merged {
		pr.MergeCommit = pull.MergeCommitSHA
		pr.SquashOn = []string{pull.Base.Ref}
	}
	for _, label := range pull.Labels {
		pr.Labels = append(pr.Labels, label.Name)
	}

	group, gctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		reviews, err := ghapi.Paginate[restReview](gctx, p.gh, fmt.Sprintf("%s/pulls/%d/reviews?per_page=100", repo, t.number))
		pr.Reviews = latestOpinions(reviews)
		return err
	})
	group.Go(func() error {
		runs, err := ghapi.Get[restCheckRuns](gctx, p.gh, fmt.Sprintf("%s/commits/%s/check-runs?per_page=100", repo, pull.Head.SHA))
		if err != nil {
			return err
		}
		status, err := ghapi.Get[restStatus](gctx, p.gh, fmt.Sprintf("%s/commits/%s/status?per_page=100", repo, pull.Head.SHA))
		pr.Rollup, pr.Status = restRollup(runs, status)
		return err
	})
	if t.activity {
		group.Go(func() error {
			comments, err := ghapi.Paginate[restComment](gctx, p.gh, fmt.Sprintf("%s/issues/%d/comments?per_page=100", repo, t.number))
			pr.Activity = ""
			for _, comment := range comments {
				if strings.HasPrefix(comment.Body, ActivityHeading) {
					pr.Activity = comment.Body
				}
			}
			return err
		})
	}
	if err := group.Wait(); err != nil {
		return PR{}, err
	}
	pr.ReviewDecision = restReviewDecision(pr.Reviews, p.prev[t.number], pull.Head.SHA)
	if t.squash == "" {
		return pr, nil
	}
	for _, branch := range []string{t.base, p.out.trunk.Name} {
		if branch == "" || slices.Contains(pr.SquashOn, branch) {
			continue
		}
		compared, err := ghapi.Get[restCompare](ctx, p.gh, fmt.Sprintf("%s/compare/%s...%s", repo, branch, t.squash))
		switch {
		case errors.Is(err, ghapi.ErrNotFound):
		case err != nil:
			return PR{}, err
		case compared.Status == "behind" || compared.Status == "identical":
			pr.SquashOn = append(pr.SquashOn, branch)
		}
	}
	return pr, nil
}

func restState(pull restPull) string {
	if pull.Merged {
		return "MERGED"
	}
	return strings.ToUpper(pull.State)
}

func restMergeable(mergeable *bool) string {
	switch {
	case mergeable == nil:
		return "UNKNOWN"
	case *mergeable:
		return "MERGEABLE"
	}
	return "CONFLICTING"
}

func latestOpinions(reviews []restReview) []Review {
	out := make([]Review, 0, len(reviews))
	at := map[string]int{}
	for _, r := range reviews {
		if r.State != "APPROVED" && r.State != "CHANGES_REQUESTED" && r.State != "DISMISSED" {
			continue
		}
		review := Review{State: r.State}
		if r.User != nil {
			review.Author = r.User.Login
		}
		if i, seen := at[review.Author]; seen {
			out[i] = review
			continue
		}
		at[review.Author], out = len(out), append(out, review)
	}
	return slices.DeleteFunc(out, func(r Review) bool { return r.State == "DISMISSED" })
}

// REST reports no reviewDecision, so it is carried from the last record of the
// same head; only the reviews themselves can say changes are requested.
func restReviewDecision(reviews []Review, last PR, head string) string {
	switch {
	case slices.ContainsFunc(reviews, func(r Review) bool { return r.State == "CHANGES_REQUESTED" }):
		return "CHANGES_REQUESTED"
	case last.HeadRefOid == head && last.ReviewDecision != "CHANGES_REQUESTED":
		return last.ReviewDecision
	}
	return ""
}

func restRollup(runs restCheckRuns, status restStatus) (*Rollup, string) {
	var combined string
	if status.TotalCount > 0 {
		combined = strings.ToUpper(status.State)
	}
	rollup := &Rollup{}
	for _, run := range runs.CheckRuns {
		rollup.Contexts.Nodes = append(rollup.Contexts.Nodes, Context{
			Typename:   "CheckRun",
			Name:       run.Name,
			Status:     strings.ToUpper(run.Status),
			Conclusion: strings.ToUpper(run.Conclusion),
		})
	}
	for _, s := range status.Statuses {
		rollup.Contexts.Nodes = append(rollup.Contexts.Nodes, Context{Typename: "StatusContext", Context: s.Context, State: strings.ToUpper(s.State)})
	}
	if len(rollup.Contexts.Nodes) == 0 {
		return nil, combined
	}
	rollup.State = rollupState(rollup.Contexts.Nodes)
	if combined == "FAILURE" || combined == "ERROR" {
		rollup.State = "FAILURE"
	}
	return rollup, combined
}

func rollupState(contexts []Context) string {
	state := "SUCCESS"
	for _, c := range contexts {
		verdict := c.State
		if c.Typename == "CheckRun" {
			verdict = c.Conclusion
			if verdict == "" {
				verdict = c.Status
			}
		}
		switch verdict {
		case "SUCCESS", "SKIPPED", "NEUTRAL":
		case "QUEUED", "IN_PROGRESS", "PENDING", "WAITING", "REQUESTED", "EXPECTED":
			state = "PENDING"
		default:
			return "FAILURE"
		}
	}
	return state
}
