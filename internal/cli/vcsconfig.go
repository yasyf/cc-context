package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/yasyf/cc-context/internal/gtapi"
)

type repositoryVCSConfig struct {
	VCS struct {
		Queue string `toml:"queue"`
	} `toml:"vcs"`
	Trunk gtapi.TrunkSSMConfig `toml:"trunk"`
}

func readRepositoryVCSConfig(root string) (repositoryVCSConfig, error) {
	var config repositoryVCSConfig
	path := filepath.Join(root, ".ccx.toml")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, fmt.Errorf("read .ccx.toml: %w", err)
	}
	if err := toml.Unmarshal(raw, &config); err != nil {
		return config, fmt.Errorf("parse .ccx.toml: %w", err)
	}
	return config, nil
}
