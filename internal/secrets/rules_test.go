package secrets

import (
	"regexp"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestRulesCompile proves every content rule in the vendored gitleaks.toml
// compiles under stdlib regexp, reporting failures per rule id.
func TestRulesCompile(t *testing.T) {
	var cfg tomlConfig
	if err := toml.Unmarshal(gitleaksTOML, &cfg); err != nil {
		t.Fatalf("parse gitleaks.toml: %v", err)
	}
	if got, want := len(cfg.Rules), 222; got != want {
		t.Fatalf("vendored rule count = %d, want %d", got, want)
	}
	kept := 0
	for _, r := range cfg.Rules {
		if r.Regex == "" {
			continue
		}
		kept++
		if _, err := regexp.Compile(r.Regex); err != nil {
			t.Errorf("rule %q does not compile: %v", r.ID, err)
		}
	}
	if want := 221; kept != want {
		t.Errorf("content rule count = %d, want %d", kept, want)
	}
	if len(rules()) != kept {
		t.Errorf("loaded rules = %d, want %d", len(rules()), kept)
	}
}

// TestLoadRulesSkipsPathOnly proves the one path-only rule in v8.30.1 is
// absent from the loaded set.
func TestLoadRulesSkipsPathOnly(t *testing.T) {
	for _, r := range rules() {
		if r.id == "pkcs12-file" {
			t.Errorf("path-only rule %q loaded; want skipped", r.id)
		}
	}
}
