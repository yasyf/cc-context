package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type cciPeer struct {
	Session   string `json:"session"`
	Agent     string `json:"agent"`
	AgentName string `json:"agent_name"`
	Drive     string `json:"drive"`
	Lane      string `json:"lane"`
	Name      string `json:"name"`
}

func (p cciPeer) self(ctx context.Context) bool {
	return p.Session == cmp.Or(render.Getenv(ctx, envClaudeSessionKey), render.Getenv(ctx, envCodexThreadKey))
}

func stackLaneHolders(ctx context.Context, l lane, branches []string) (map[string]string, error) {
	holders, err := vcs.ForeignBranchHolders(ctx, l.checkout)
	if err != nil {
		return nil, err
	}
	claims, err := stackClaims(ctx, l.dir(), branches)
	if err != nil {
		return nil, err
	}
	return stackWithClaims(holders, claims), nil
}

func stackClaims(ctx context.Context, dir render.Dir, branches []string) (map[string]string, error) {
	if render.LookPath(ctx, "cci") == "" {
		return nil, nil
	}
	current, err := gitCurrentBranch(ctx, dir, "stack")
	if err != nil {
		return nil, err
	}
	_, me, err := cciDriveLane(ctx, dir)
	if err != nil {
		return nil, err
	}
	claims := map[string]string{}
	for _, branch := range branches {
		if branch == current {
			continue
		}
		owner, err := cciBranchOwner(ctx, dir, branch, me)
		if err != nil {
			return nil, err
		}
		if owner != "" {
			claims[branch] = "lane " + owner
		}
	}
	if len(claims) == 0 {
		return claims, nil
	}
	here, err := stackCheckedOutHere(ctx, dir, "stack")
	if err != nil {
		return nil, err
	}
	maps.DeleteFunc(claims, func(branch, _ string) bool { return here[branch] })
	return claims, nil
}

func stackCheckedOutHere(ctx context.Context, dir render.Dir, prefix string) (map[string]bool, error) {
	current, err := gitCurrentBranch(ctx, dir, prefix)
	if err != nil {
		return nil, err
	}
	out, err := render.RunCLI(ctx, dir, "git", []string{"reflog", "show", "--format=%gs", "HEAD"})
	if err != nil {
		return nil, fmt.Errorf("%s: git reflog show HEAD: %w", prefix, err)
	}
	here := map[string]bool{}
	if current != "" {
		here[current] = true
	}
	for line := range strings.Lines(out) {
		line = strings.TrimSpace(line)
		if moved, ok := strings.CutPrefix(line, "checkout: moving from "); ok {
			from, to, _ := strings.Cut(moved, " to ")
			here[from], here[to] = true, true
		} else if branch, ok := strings.CutPrefix(line, "rebase (finish): returning to refs/heads/"); ok {
			here[branch] = true
		}
	}
	return here, nil
}

func stackOtherLanes(ctx context.Context, dir render.Dir, prefix, current string, names []string) (map[string]bool, error) {
	others := map[string]bool{}
	for _, name := range names {
		if branchLane(name) != branchLane(current) {
			others[name] = true
		}
	}
	if len(others) == 0 {
		return others, nil
	}
	here, err := stackCheckedOutHere(ctx, dir, prefix)
	if err != nil {
		return nil, err
	}
	maps.DeleteFunc(others, func(name string, _ bool) bool { return here[name] })
	return others, nil
}

func stackWithClaims(holders, claims map[string]string) map[string]string {
	for branch, owner := range claims {
		if holders[branch] == "" {
			holders[branch] = owner
		}
	}
	return holders
}

func cciBranchOwner(ctx context.Context, dir render.Dir, branch, me string) (string, error) {
	out, err := cciWho(ctx, dir, "--ref", "branch:"+branch, "--json")
	if err != nil {
		return "", err
	}
	for line := range strings.Lines(out) {
		var p cciPeer
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return "", fmt.Errorf("cci who --ref branch:%s: %w", branch, err)
		}
		if !p.self(ctx) && !laneCarries(me, p.Lane) && !laneCarries(me, p.AgentName) {
			return cmp.Or(p.Lane, p.AgentName, p.Name, p.Session), nil
		}
	}
	return "", nil
}

var laneNumber = regexp.MustCompile(`^(.+?)(?:-(\d+))?$`)

func laneCarries(me, other string) bool {
	if me == "" || other == "" {
		return false
	}
	if me == other {
		return true
	}
	mine, theirs := laneNumber.FindStringSubmatch(me), laneNumber.FindStringSubmatch(other)
	if mine[1] != theirs[1] || mine[2] == "" {
		return false
	}
	n, _ := strconv.Atoi(mine[2])
	m := 1
	if theirs[2] != "" {
		m, _ = strconv.Atoi(theirs[2])
	}
	return m < n
}

func stackHolderPhrase(holder string) string {
	if filepath.IsAbs(holder) {
		return gtHeldElsewhere + holder
	}
	return "claimed by " + holder
}

func stackOwnerName(holder string) string {
	if filepath.IsAbs(holder) {
		return "the working copy at " + holder
	}
	return holder
}
