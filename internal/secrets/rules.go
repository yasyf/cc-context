// Package secrets detects and masks secret material in text using gitleaks'
// default detection rules.
//
// gitleaks.toml is vendored verbatim from https://github.com/gitleaks/gitleaks
// (config/gitleaks.toml, tag v8.30.1). It is MIT-licensed, Copyright (c) 2019
// Zachary Rice; the full license is at
// https://github.com/gitleaks/gitleaks/blob/v8.30.1/LICENSE. Only id, regex,
// secretGroup, entropy, and keywords are consumed; path-only rules are skipped
// and allowlists are not applied.
package secrets

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
)

//go:embed gitleaks.toml
var gitleaksTOML []byte

// rule is one compiled gitleaks detection rule.
type rule struct {
	id          string
	re          *regexp.Regexp
	secretGroup int
	entropy     float64
	keywords    []string
}

// tomlConfig mirrors the slice of the gitleaks config Mask consumes; every
// other key in the vendored file is ignored.
type tomlConfig struct {
	Rules []tomlRule `toml:"rules"`
}

type tomlRule struct {
	ID          string   `toml:"id"`
	Regex       string   `toml:"regex"`
	SecretGroup int      `toml:"secretGroup"`
	Entropy     float64  `toml:"entropy"`
	Keywords    []string `toml:"keywords"`
}

// rules returns the compiled detection rules, building them on first mask and
// caching for the process — secrets links into every command via render, so the
// ~14–32ms compile is deferred off init. A malformed asset still panics, now on
// first read.
var rules = sync.OnceValue(func() []rule { return loadRules(gitleaksTOML) })

func loadRules(raw []byte) []rule {
	var cfg tomlConfig
	if err := toml.Unmarshal(raw, &cfg); err != nil {
		panic(fmt.Sprintf("secrets: parse embedded gitleaks.toml: %v", err))
	}
	out := make([]rule, 0, len(cfg.Rules))
	for _, r := range cfg.Rules {
		if r.Regex == "" {
			continue
		}
		re, err := regexp.Compile(r.Regex)
		if err != nil {
			panic(fmt.Sprintf("secrets: compile rule %q: %v", r.ID, err))
		}
		if r.SecretGroup > re.NumSubexp() {
			panic(fmt.Sprintf("secrets: rule %q secretGroup %d exceeds %d subexpression(s)", r.ID, r.SecretGroup, re.NumSubexp()))
		}
		kws := make([]string, len(r.Keywords))
		for i, kw := range r.Keywords {
			kws[i] = strings.ToLower(kw)
		}
		out = append(out, rule{id: r.ID, re: re, secretGroup: r.SecretGroup, entropy: r.Entropy, keywords: kws})
	}
	return out
}
