package gtapi

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoToken, err)
	}
	path := filepath.Join(home, ".config", "graphite", "auth")
	payload, err := os.ReadFile(path) //nolint:gosec // the path is gt's own auth file under the user's home
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoToken, err)
	}
	var auth struct {
		AuthToken string `json:"authToken"`
	}
	if err := json.Unmarshal(payload, &auth); err != nil {
		return "", fmt.Errorf("%w: decode %s: %w", ErrNoToken, path, err)
	}
	if auth.AuthToken == "" {
		return "", fmt.Errorf("%w: %s carries no authToken", ErrNoToken, path)
	}
	return auth.AuthToken, nil
}
