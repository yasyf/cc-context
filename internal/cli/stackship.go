package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
)

type stackShipMeta struct {
	Title string  `json:"title,omitempty"`
	Body  *string `json:"body,omitempty"`
	Draft *bool   `json:"draft,omitempty"`
}

type stackShipIntent struct {
	Repo    string                   `json:"repo"`
	Branch  string                   `json:"branch"`
	Meta    map[string]stackShipMeta `json:"meta"`
	NoWatch bool                     `json:"no_watch"`
	Reviews bool                     `json:"reviews"`
	Budget  int                      `json:"budget"`
}

func stackShipOptions(o shipOpts, meta map[string]prMeta, repo, branch string) (*stackShipIntent, error) {
	intent := &stackShipIntent{Repo: repo, Branch: branch, Meta: map[string]stackShipMeta{}, NoWatch: o.noWatch, Reviews: o.reviews, Budget: o.budget}
	for name, m := range meta {
		saved := stackShipMeta{Title: m.title, Draft: m.draft}
		if m.bodyPath != "" {
			body, err := os.ReadFile(m.bodyPath)
			if err != nil {
				return nil, err
			}
			text := string(body)
			saved.Body = &text
		}
		intent.Meta[name] = saved
	}
	return intent, nil
}

func stackFinishShip(ctx context.Context, cmd *cobra.Command, l lane, run *stackRebaseRun, submitted map[string]stackEntry) error {
	intent := run.Ship
	meta := map[string]prMeta{}
	for name, saved := range intent.Meta {
		m := prMeta{title: saved.Title, draft: saved.Draft}
		if saved.Body != nil {
			m.bodyPath = filepath.Join(run.dir, "body-"+strconv.Itoa(len(meta)))
			if err := os.WriteFile(m.bodyPath, []byte(*saved.Body), 0o600); err != nil {
				return err
			}
		}
		meta[name] = m
	}
	var chain []string
	for i := len(run.Branches) - 1; i >= 0; i-- {
		if b := run.Branches[i]; b.Landed == "" && b.Held == "" {
			chain = append(chain, b.Name)
		}
	}
	_, _, entries := gtPRSegment(intent.Branch, chain, meta, submitted)
	if len(meta) > 0 {
		segment, err := shipPRGT(ctx, intent.Repo, meta, entries)
		if err != nil {
			return err
		}
		if segment != "" {
			cmd.Println(segment)
		}
	}
	if !intent.NoWatch {
		segment, report, err := shipWatchCIHead(ctx, cmd.ErrOrStderr(), l.dir(), run.branch(intent.Branch).NewHead, intent.Budget)
		if segment != "" {
			cmd.Println(segment)
		}
		for _, line := range report {
			cmd.Println(line)
		}
		if err != nil {
			return err
		}
	}
	if intent.Reviews {
		return shipReviewsWatch(ctx, cmd.OutOrStdout(), chain)
	}
	return nil
}
