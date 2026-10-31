package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
)

var cciWhoTimeout = 20 * time.Second

const (
	envCCIDrive = "CCI_DRIVE"
	envCCILane  = "CCI_LANE"
	envCCIAgent = "CCI_AGENT_ID"
)

type cciHeads struct {
	drive   string
	lane    string
	tracked map[string]string
}

func cciHeadsBeforePush(ctx context.Context, errW io.Writer, dir render.Dir, prefix string, plan []gtSubmitBranch) *cciHeads {
	h, err := cciHeadsOf(ctx, dir, plan)
	if err != nil {
		_, _ = fmt.Fprintf(errW, "%s: cci HEAD records: %v\n", prefix, err)
	}
	return h
}

func cciHeadsOf(ctx context.Context, dir render.Dir, plan []gtSubmitBranch) (*cciHeads, error) {
	drive, lane, err := cciDriveLane(ctx, dir)
	if err != nil || drive == "" || lane == "" {
		return nil, err
	}
	refs := make([]string, len(plan))
	for i, b := range plan {
		refs[i] = "refs/remotes/origin/" + b.name
	}
	out, err := render.RunCLI(ctx, dir, "git", append([]string{"for-each-ref", "--format=%(refname:lstrip=3) %(objectname)"}, refs...))
	if err != nil {
		return nil, err
	}
	h := &cciHeads{drive: drive, lane: lane, tracked: map[string]string{}}
	for line := range strings.Lines(out) {
		if name, sha, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			h.tracked[name] = sha
		}
	}
	return h, nil
}

func cciDriveLane(ctx context.Context, dir render.Dir) (string, string, error) {
	if render.LookPath(ctx, "cci") == "" {
		return "", "", nil
	}
	drive, lane := render.Getenv(ctx, envCCIDrive), render.Getenv(ctx, envCCILane)
	session := cmp.Or(render.Getenv(ctx, envClaudeSessionKey), render.Getenv(ctx, envCodexThreadKey))
	if (drive != "" && lane != "") || session == "" {
		return drive, lane, nil
	}
	out, err := cciWho(ctx, dir, "--session", session, "--json")
	if err != nil {
		return "", "", err
	}
	agent := render.Getenv(ctx, envCCIAgent)
	for line := range strings.Lines(out) {
		var p cciPeer
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return "", "", fmt.Errorf("cci who --session %s: %w", session, err)
		}
		if p.Agent == agent {
			return cmp.Or(drive, p.Drive), cmp.Or(lane, p.Lane), nil
		}
	}
	return drive, lane, nil
}

func cciWho(ctx context.Context, dir render.Dir, argv ...string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, cciWhoTimeout)
	defer cancel()
	out, err := render.RunCLI(bounded, dir, "cci", append([]string{"who"}, argv...))
	call := strings.Join(append([]string{"cci who"}, argv...), " ")
	if bounded.Err() != nil && ctx.Err() == nil {
		return "", fmt.Errorf("%s: no answer in %s; rerun once cci answers", call, cciWhoTimeout)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", call, err)
	}
	return out, nil
}

func (h *cciHeads) post(ctx context.Context, errW io.Writer, dir render.Dir, s gtSubmit, repo string, plan []gtSubmitBranch, known map[string]gtapi.PullRequestInfo) {
	if h == nil {
		return
	}
	var heads, keys []string
	for _, b := range plan {
		was := h.tracked[b.name]
		if b.lease != "" || b.leaseSet {
			was = b.lease
		}
		if b.pr == 0 || was == b.head {
			continue
		}
		verb, summary := "ship", "none: origin held no head"
		if was != "" {
			var err error
			if summary, err = cciRangeDiff(ctx, dir, s.prefix, was, known[b.name].Newest(), b); err != nil {
				_, _ = fmt.Fprintf(errW, "%s: cci HEAD #%d: %v\n", s.prefix, b.pr, err)
				summary = "unavailable"
			}
		}
		if s.publication != nil {
			verb = "replay"
			if strings.HasPrefix(summary, rangeDiffIdentical) {
				verb = "restack"
			}
		}
		pr, moved := repo+"#"+strconv.Itoa(b.pr), b.head
		if was != "" {
			moved = was + ".." + b.head
		}
		heads = append(heads, "--head", fmt.Sprintf("%s=%s %s, range-diff %s", pr, moved, verb, summary))
		keys = append(keys, pr+":"+b.head)
	}
	if len(keys) == 0 {
		return
	}
	argv := append([]string{"post", "--drive", h.drive, "--lane", h.lane, "--kind", "head", "--id", "head:" + strings.Join(keys, ",")}, heads...)
	if _, err := render.RunCLI(ctx, dir, "cci", argv); err != nil {
		_, _ = fmt.Fprintf(errW, "%s: cci post HEAD %s: %v\n", s.prefix, strings.Join(keys, ", "), err)
	}
}

const rangeDiffIdentical = "patch-identical"

func cciRangeDiff(ctx context.Context, dir render.Dir, prefix, was string, last gtapi.PRVersion, b gtSubmitBranch) (string, error) {
	oldBase := last.BaseSha
	if last.HeadSha != was || oldBase == "" {
		var err error
		if oldBase, err = gtMergeBase(ctx, dir, prefix, was, b.baseSha); err != nil {
			return "", err
		}
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{"range-diff", "--no-color", "--no-patch", oldBase + ".." + was, b.baseSha + ".." + b.head})
	if err != nil {
		return "", fmt.Errorf("git range-diff: %w", err)
	}
	return rangeDiffSummary(out), nil
}

var rangeDiffPair = regexp.MustCompile(`^\s*(?:\d+|-+):\s+(?:[0-9a-f]+|-+)\s+([=!<>])\s+(?:\d+|-+):\s+([0-9a-f]+|-+)`)

func rangeDiffSummary(out string) string {
	counts := map[string]int{}
	var added []string
	for line := range strings.Lines(out) {
		pair := rangeDiffPair.FindStringSubmatch(line)
		if pair == nil {
			continue
		}
		counts[pair[1]]++
		if pair[1] == ">" {
			added = append(added, pair[2])
		}
	}
	if counts["!"]+counts["<"]+counts[">"] == 0 {
		return fmt.Sprintf("%s (%d %s)", rangeDiffIdentical, counts["="], plural(counts["="], "commit", "commits"))
	}
	var parts []string
	for _, m := range []struct{ mark, name string }{{"=", "identical"}, {"!", "changed"}, {">", "added"}, {"<", "dropped"}} {
		if n := counts[m.mark]; n > 0 {
			part := fmt.Sprintf("%d %s", n, m.name)
			if m.mark == ">" {
				part += " (" + strings.Join(added, ", ") + ")"
			}
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, ", ")
}
