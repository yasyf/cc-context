package nativeapi

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
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
