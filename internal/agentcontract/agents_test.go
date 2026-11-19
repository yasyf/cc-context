package agentcontract

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

type agent struct {
	fields map[string]string
	body   string
}

func readAgents(t *testing.T) map[string]agent {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	dir := filepath.Join(filepath.Dir(source), "..", "..", "plugin", "agents")
	paths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatalf("no agent files under %s", dir)
	}
	agents := map[string]agent{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		front, body, ok := strings.Cut(strings.TrimPrefix(string(data), "---\n"), "\n---\n")
		if !ok {
			t.Fatalf("%s: no frontmatter", path)
		}
		fields := map[string]string{}
		for line := range strings.SplitSeq(front, "\n") {
			key, value, ok := strings.Cut(line, ":")
			if !ok {
				t.Fatalf("%s: frontmatter line %q has no key", path, line)
			}
			fields[key] = strings.TrimSpace(value)
		}
		agents[strings.TrimSuffix(filepath.Base(path), ".md")] = agent{fields: fields, body: body}
	}
	return agents
}

func TestAgentsDeclareTheirContract(t *testing.T) {
	for stem, a := range readAgents(t) {
		if a.fields["name"] != stem {
			t.Errorf("%s: name %q does not match its file", stem, a.fields["name"])
		}
		for _, key := range []string{"description", "tools", "model"} {
			if a.fields[key] == "" {
				t.Errorf("%s: frontmatter has no %s", stem, key)
			}
		}
		if _, ok := a.fields["maxTurns"]; ok {
			t.Errorf("%s: sets maxTurns; a subagent stopped at its turn limit hands its caller no report, so bound the work in the prompt", stem)
		}
	}
}

func TestEnumeratorReturnsWithinItsBudget(t *testing.T) {
	enumerator := readAgents(t)["enumerator"]
	match := regexp.MustCompile(`budget of (\d+) tool calls`).FindStringSubmatch(enumerator.body)
	if match == nil {
		t.Fatal("enumerator states no tool-call budget")
	}
	if calls, _ := strconv.Atoi(match[1]); calls > 40 {
		t.Errorf("enumerator budget = %d tool calls, want at most 40: at about ten seconds a call, more blocks its caller for many minutes", calls)
	}
	for _, section := range []string{"**Unverified**", "**Not covered**"} {
		if !strings.Contains(enumerator.body, section) {
			t.Errorf("enumerator return shape has no %s section for what its budget left out", section)
		}
	}
}
