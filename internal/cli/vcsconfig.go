package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/BurntSushi/toml"

	"github.com/yasyf/cc-context/internal/gtapi"
)

type repositoryVCSConfig struct {
	VCS struct {
		Queue string `toml:"queue"`
	} `toml:"vcs"`
	Trunk     gtapi.TrunkSSMConfig `toml:"trunk"`
	Generated []regenGenerator     `toml:"generated"`
}

func readRepositoryVCSConfig(root string) (repositoryVCSConfig, error) {
	var config repositoryVCSConfig
	directory, err := os.OpenRoot(root)
	if err != nil {
		return config, fmt.Errorf("open repository configuration root: %w", err)
	}
	defer func() { _ = directory.Close() }()
	raw, err := directory.ReadFile(regenFile)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, fmt.Errorf("read .claude/ccx.toml: %w", err)
	}
	if err := toml.Unmarshal(raw, &config); err != nil {
		return config, fmt.Errorf("parse .claude/ccx.toml: %w", err)
	}
	return config, nil
}
