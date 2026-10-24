package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const envCCIAgentKey = "CCI_AGENT_ID"

type cciPeer struct {
	Session   string `json:"session"`
	Agent     string `json:"agent"`
	AgentName string `json:"agent_name"`
	Lane      string `json:"lane"`
	Name      string `json:"name"`
}

func (p cciPeer) self(ctx context.Context) bool {
	session := cmp.Or(render.Getenv(ctx, envClaudeSessionKey), render.Getenv(ctx, envCodexThreadKey))
	return p.Session == session && p.Agent == render.Getenv(ctx, envCCIAgentKey)
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
	claims := map[string]string{}
	for _, branch := range branches {
		if branch == current {
			continue
		}
		owner, err := cciBranchOwner(ctx, dir, branch)
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

func cciBranchOwner(ctx context.Context, dir render.Dir, branch string) (string, error) {
	out, err := render.RunCLI(ctx, dir, "cci", []string{"who", "--ref", "branch:" + branch, "--json"})
	if err != nil {
		return "", fmt.Errorf("cci who --ref branch:%s: %w", branch, err)
	}
	for line := range strings.Lines(out) {
		var p cciPeer
		if err := json.Unmarshal([]byte(line), &p); err != nil {
			return "", fmt.Errorf("cci who --ref branch:%s: %w", branch, err)
		}
		if !p.self(ctx) {
			return cmp.Or(p.Lane, p.AgentName, p.Name, p.Session), nil
		}
	}
	return "", nil
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
