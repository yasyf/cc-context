package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
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
	return claims, nil
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
	out, err := render.RunCLI(ctx, dir, "cci", []string{"who", "--ref", "branch:" + branch, "--json"})
	if err != nil {
		return "", fmt.Errorf("cci who --ref branch:%s: %w", branch, err)
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
