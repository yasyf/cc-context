package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

type repositoryVCSConfig struct {
	VCS struct {
		Queue string `toml:"queue"`
		Stack string `toml:"stack"`
	} `toml:"vcs"`
	Trunk     gtapi.TrunkSSMConfig `toml:"trunk"`
	Generated []regenGenerator     `toml:"generated"`
}

func readRepositoryVCSConfig(ctx context.Context, root string) (repositoryVCSConfig, error) {
	var config repositoryVCSConfig
	directory, err := os.OpenRoot(root)
	if err != nil {
		return config, fmt.Errorf("open repository configuration root: %w", err)
	}
	defer func() { _ = directory.Close() }()
	raw, err := directory.ReadFile(regenFile)
	if errors.Is(err, os.ErrNotExist) {
		trunk, err := vcs.ResolveTrunk(ctx, render.Dir(root), "origin")
		if errors.Is(err, vcs.ErrNoTrunk) {
			return config, nil
		}
		if err != nil {
			return config, fmt.Errorf("resolve repository configuration branch: %w", err)
		}
		listed, err := render.RunCLI(ctx, render.Dir(root), "git", []string{"ls-tree", "--full-tree", "--name-only", string(trunk.Ref()), "--", regenFile})
		if err != nil {
			return config, fmt.Errorf("look for %s on %s: %w", regenFile, trunk.Ref(), err)
		}
		if strings.TrimSpace(listed) == "" {
			return config, nil
		}
		committed, err := render.RunCLI(ctx, render.Dir(root), "git", []string{"show", string(trunk.Ref()) + ":" + regenFile})
		if err != nil {
			return config, fmt.Errorf("read %s on %s: %w", regenFile, trunk.Ref(), err)
		}
		raw = []byte(committed)
	} else if err != nil {
		return config, fmt.Errorf("read .claude/ccx.toml: %w", err)
	}
	if err := toml.Unmarshal(raw, &config); err != nil {
		return config, fmt.Errorf("parse .claude/ccx.toml: %w", err)
	}
	return config, nil
}
