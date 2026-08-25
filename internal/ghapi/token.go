package ghapi

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/yasyf/cc-context/internal/lookpath"
	"github.com/yasyf/cc-context/internal/render"
)

// envTokens are the token env vars gh itself reads, in gh's precedence order,
// before falling back to its own credential store.
var envTokens = []string{"GH_TOKEN", "GITHUB_TOKEN"}

// tokenSource caches its client's token: gh's store is keyring-backed, so the
// only way to read it is to spawn gh, which a watch must not do per request.
type tokenSource struct {
	mu      sync.Mutex
	token   string
	resolve func(context.Context) (string, error)
}

func (s *tokenSource) get(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" {
		return s.token, nil
	}
	token, err := s.resolve(ctx)
	if err != nil {
		return "", err
	}
	s.token = token
	return token, nil
}

// refresh re-resolves the token unless another caller already replaced stale —
// the token that just drew a 401 — so concurrent 401s cost one resolution.
func (s *tokenSource) refresh(ctx context.Context, stale string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != stale {
		return nil
	}
	token, err := s.resolve(ctx)
	if err != nil {
		return err
	}
	s.token = token
	return nil
}

// resolveToken reads GH_TOKEN, then GITHUB_TOKEN, then `gh auth token` — gh's
// own precedence. The subprocess is the one place ccx shells out for auth; gh
// stays the sole authority over the credential store.
func resolveToken(ctx context.Context) (string, error) {
	for _, name := range envTokens {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			return token, nil
		}
	}
	gh := lookpath.Find("gh")
	if gh == "" {
		return "", fmt.Errorf("%w: gh is not on PATH and neither GH_TOKEN nor GITHUB_TOKEN is set", ErrNoToken)
	}
	out, err := render.RunCLI(ctx, render.Ambient, gh, []string{"auth", "token"})
	if err != nil {
		return "", fmt.Errorf("%w: gh auth token: %w", ErrNoToken, err)
	}
	token := strings.TrimSpace(out)
	if token == "" {
		return "", fmt.Errorf("%w: gh auth token printed nothing", ErrNoToken)
	}
	return token, nil
}
