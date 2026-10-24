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
	session := p.Session == render.Getenv(ctx, envClaudeSessionKey) || p.Session == render.Getenv(ctx, envCodexThreadKey)
	return session && p.Agent == render.Getenv(ctx, envCCIAgentKey)
}

func stackLaneHolders(ctx context.Context, l lane, branches []string) (map[string]string, error) {
	holders, err := vcs.ForeignBranchHolders(ctx, l.checkout)
	if err != nil {
		return nil, err
	}
	if err := stackAddClaims(ctx, l.dir(), holders, branches); err != nil {
		return nil, err
	}
	return holders, nil
}

func stackAddClaims(ctx context.Context, dir render.Dir, holders map[string]string, branches []string) error {
	if !cciClaimable(ctx) {
		return nil
	}
	for _, branch := range branches {
		if holders[branch] != "" {
			continue
		}
		owner, err := cciBranchOwner(ctx, dir, branch)
		if err != nil {
			return err
		}
		if owner != "" {
			holders[branch] = "lane " + owner
		}
	}
	return nil
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
