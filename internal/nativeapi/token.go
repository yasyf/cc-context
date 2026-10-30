package nativeapi

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

type tokenSource struct {
	mu      sync.Mutex
	token   string
	resolve func() (string, error)
}

func (s *tokenSource) get() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" {
		return s.token, nil
	}
	token, err := s.resolve()
	if err != nil {
		return "", err
	}
	s.token = token
	return token, nil
}

func resolveToken() (string, error) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if token := os.Getenv(key); token != "" {
			return token, nil
		}
	}
	out, err := exec.Command("gh", "auth", "token", "--hostname", "github.com").Output()
	if err != nil {
		return "", fmt.Errorf("%w: gh auth token: %w", ErrNoToken, err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("%w: gh auth token returned an empty token", ErrNoToken)
	}
	return token, nil
}

type trunkConfig struct {
	APIKeyCommand []string `toml:"api_key_command"`
}

// trunkConfigPath is the file naming the command that prints the Trunk API key:
// $XDG_CONFIG_HOME/ccx/trunk.toml, else ~/.config/ccx/trunk.toml.
func trunkConfigPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "ccx", "trunk.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("trunk: resolve home: %w", err)
	}
	return filepath.Join(home, ".config", "ccx", "trunk.toml"), nil
}

func resolveTrunkKey() (string, error) {
	if token := os.Getenv("TRUNK_MERGE_API_KEY"); token != "" {
		return token, nil
	}
	path, err := trunkConfigPath()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(path) //nolint:gosec // the user's own ccx config file
	if errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("trunk queue status requires TRUNK_MERGE_API_KEY or an api_key_command in %s printing a scoped v2 API key", path)
	}
	if err != nil {
		return "", fmt.Errorf("trunk: read %s: %w", path, err)
	}
	var cfg trunkConfig
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		return "", fmt.Errorf("trunk: parse %s: %w", path, err)
	}
	if len(cfg.APIKeyCommand) == 0 {
		return "", fmt.Errorf("trunk: %s needs api_key_command", path)
	}
	out, err := exec.Command(cfg.APIKeyCommand[0], cfg.APIKeyCommand[1:]...).Output() //nolint:gosec // the user's own configured command
	if err != nil {
		return "", fmt.Errorf("trunk: %s api_key_command: %w", path, err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("trunk: %s api_key_command printed no key", path)
	}
	return token, nil
}
