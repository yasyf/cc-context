package nativeapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"unicode"
)

// TrunkSSMConfig locates a scoped Trunk queue credential in AWS Parameter Store.
type TrunkSSMConfig struct {
	SSMParameter string `toml:"ssm_parameter"`
	AWSProfile   string `toml:"aws_profile"`
	AWSRegion    string `toml:"aws_region"`
}

type trunkSSMSource struct {
	config    TrunkSSMConfig
	configErr error
	gate      chan struct{}
	token     string
}

var trunkSSMSources sync.Map

// WithTrunkSSM supplies lazy AWS credential lookup for selected Trunk requests.
func (c *Client) WithTrunkSSM(config TrunkSSMConfig) *Client {
	selected := *c
	source := &trunkSSMSource{config: config, gate: make(chan struct{}, 1)}
	for _, field := range []struct{ name, value string }{
		{"ssm_parameter", config.SSMParameter},
		{"aws_profile", config.AWSProfile},
		{"aws_region", config.AWSRegion},
	} {
		if field.value == "" || strings.TrimSpace(field.value) != field.value || strings.ContainsFunc(field.value, unicode.IsControl) || strings.HasPrefix(field.value, "-") {
			source.configErr = fmt.Errorf("trunk credential configuration requires a valid %s", field.name)
			break
		}
	}
	if strings.ContainsFunc(config.SSMParameter, unicode.IsSpace) || strings.ContainsFunc(config.AWSRegion, unicode.IsSpace) {
		source.configErr = fmt.Errorf("trunk credential configuration ssm_parameter and aws_region cannot contain whitespace")
	}
	shared, _ := trunkSSMSources.LoadOrStore(config, source)
	selected.trunkSSM = shared.(*trunkSSMSource)
	return &selected
}

func (c *Client) trunkToken(ctx context.Context) (string, error) {
	if token := os.Getenv("TRUNK_MERGE_API_KEY"); token != "" {
		return token, nil
	}
	if c.trunkSSM == nil || c.trunkSSM.config == (TrunkSSMConfig{}) {
		return "", fmt.Errorf("trunk queue status requires TRUNK_MERGE_API_KEY or [trunk] ssm_parameter, aws_profile, and aws_region")
	}
	return c.trunkSSM.get(ctx)
}

func (s *trunkSSMSource) get(ctx context.Context) (string, error) {
	if s.configErr != nil {
		return "", s.configErr
	}
	select {
	case s.gate <- struct{}{}:
		defer func() { <-s.gate }()
	case <-ctx.Done():
		return "", fmt.Errorf("trunk credential lookup canceled: %w", ctx.Err())
	}
	if err := ctx.Err(); err != nil {
		return "", fmt.Errorf("trunk credential lookup canceled: %w", err)
	}
	if s.token != "" {
		return s.token, nil
	}
	cmd := exec.CommandContext(ctx, "aws", "ssm", "get-parameter", "--name", s.config.SSMParameter, "--with-decryption", "--profile", s.config.AWSProfile, "--region", s.config.AWSRegion, "--query", "Parameter.Value", "--output", "text") //nolint:gosec // fixed aws subcommand and flags; validated config values occupy separate argv entries without a shell
	cmd.Stderr = io.Discard
	output, err := cmd.Output()
	if err != nil {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("trunk credential lookup canceled: %w", err)
		}
		var failure *exec.ExitError
		if errors.As(err, &failure) {
			return "", fmt.Errorf("trunk credential lookup: aws ssm get-parameter failed with exit code %d; check the configured AWS profile, region, and parameter access", failure.ExitCode())
		}
		return "", fmt.Errorf("trunk credential lookup: unable to execute aws CLI")
	}
	token := strings.TrimSpace(string(output))
	if token == "" || token == "None" || token == "null" || strings.ContainsFunc(token, unicode.IsSpace) || strings.ContainsFunc(token, unicode.IsControl) {
		return "", fmt.Errorf("trunk credential lookup: AWS parameter did not contain a valid scoped API key")
	}
	s.token = token
	return token, nil
}
