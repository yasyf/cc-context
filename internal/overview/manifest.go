package overview

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// manifest is a build/dependency file found at the repo root: its filename, a
// headline for the top overview line (empty when the format contributes none), and a
// direct-dependency count when the format is parsed for one.
type manifest struct {
	file        string
	headline    string
	deps        int
	depsCounted bool
}

// manifestProbes lists the root manifests in headline priority: the first present one
// drives the overview header, and each present one contributes a manifests-line entry.
var manifestProbes = []struct {
	file  string
	parse func(file, data string) manifest
}{
	{"go.mod", parseGoMod},
	{"package.json", parsePackageJSON},
	{"pyproject.toml", parsePyproject},
	{"Cargo.toml", parseCargo},
	{"composer.json", parseComposer},
	{"Gemfile", parseGemfile},
	{"pom.xml", parsePomXML},
	{"build.gradle", parseGradle},
	{"build.gradle.kts", parseGradle},
}

// probeManifests reads and parses each root manifest present, in probe order.
func probeManifests(root string) []manifest {
	var out []manifest
	for _, p := range manifestProbes {
		data, err := os.ReadFile(filepath.Join(root, p.file)) //nolint:gosec // p.file comes from the fixed manifestProbes table
		if err != nil {
			continue
		}
		out = append(out, p.parse(p.file, string(data)))
	}
	return out
}

// manifestsLine renders "manifests: go.mod (14 direct deps) · Taskfile.yml" from the
// probed manifests, appending the dep count only for formats that were counted.
func manifestsLine(ms []manifest) string {
	if len(ms) == 0 {
		return ""
	}
	parts := make([]string, len(ms))
	for i, m := range ms {
		if m.depsCounted {
			parts[i] = m.file + " (" + strconv.Itoa(m.deps) + " direct deps)"
		} else {
			parts[i] = m.file
		}
	}
	return "manifests: " + strings.Join(parts, " · ")
}

// parseGoMod extracts the module path, Go version, and direct-dependency count (the
// require entries not marked "// indirect") from go.mod.
func parseGoMod(file, data string) manifest {
	m := manifest{file: file, depsCounted: true}
	var modulePath, goVer string
	inRequire := false
	for _, ln := range strings.Split(data, "\n") {
		t := strings.TrimSpace(ln)
		switch {
		case t == "require (":
			inRequire = true
		case inRequire && t == ")":
			inRequire = false
		case inRequire:
			if t != "" && !strings.HasPrefix(t, "//") && !strings.Contains(t, "// indirect") {
				m.deps++
			}
		case strings.HasPrefix(t, "module "):
			modulePath = strings.TrimSpace(t[len("module "):])
		case goVer == "" && strings.HasPrefix(t, "go "):
			goVer = strings.TrimSpace(t[len("go "):])
		case strings.HasPrefix(t, "require ") && !strings.Contains(t, "// indirect"):
			m.deps++
		}
	}
	m.headline = "go module " + modulePath
	if v := shortGoVersion(goVer); v != "" {
		m.headline += " (go " + v + ")"
	}
	return m
}

// shortGoVersion keeps the major.minor of a Go version ("1.26.5" → "1.26").
func shortGoVersion(v string) string {
	if v == "" {
		return ""
	}
	if parts := strings.Split(v, "."); len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

// parsePackageJSON counts the dependencies and devDependencies of a package.json and
// reads its name.
func parsePackageJSON(file, data string) manifest {
	var p struct {
		Name            string                     `json:"name"`
		Dependencies    map[string]json.RawMessage `json:"dependencies"`
		DevDependencies map[string]json.RawMessage `json:"devDependencies"`
	}
	_ = json.Unmarshal([]byte(data), &p)
	return manifest{
		file:        file,
		headline:    headlineWithName("node package", p.Name),
		deps:        len(p.Dependencies) + len(p.DevDependencies),
		depsCounted: true,
	}
}

// parseComposer counts the require and require-dev entries of a composer.json and
// reads its name.
func parseComposer(file, data string) manifest {
	var p struct {
		Name       string                     `json:"name"`
		Require    map[string]json.RawMessage `json:"require"`
		RequireDev map[string]json.RawMessage `json:"require-dev"`
	}
	_ = json.Unmarshal([]byte(data), &p)
	return manifest{
		file:        file,
		headline:    headlineWithName("php package", p.Name),
		deps:        len(p.Require) + len(p.RequireDev),
		depsCounted: true,
	}
}

// parsePyproject reads the project name and counts direct dependencies of a
// pyproject.toml via a line scan, handling both PEP 621 (a [project] dependencies
// array) and Poetry (a [tool.poetry.dependencies] table, excluding python).
func parsePyproject(file, data string) manifest {
	name := ""
	deps := 0
	section := ""
	inArray := false
	for _, raw := range strings.Split(data, "\n") {
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if inArray {
			frag := t
			if i := strings.IndexByte(t, ']'); i >= 0 {
				frag = t[:i]
				inArray = false
			}
			deps += countQuoted(frag)
			continue
		}
		if h, ok := tomlSection(t); ok {
			section = h
			continue
		}
		if name == "" && (section == "project" || section == "tool.poetry") {
			if v, ok := tomlValue(t, "name"); ok {
				name = v
			}
		}
		if section == "project" {
			if rest, ok := cutKey(t, "dependencies"); ok && strings.HasPrefix(rest, "[") {
				body := rest[1:]
				if i := strings.IndexByte(body, ']'); i >= 0 {
					deps += countQuoted(body[:i])
				} else {
					deps += countQuoted(body)
					inArray = true
				}
			}
		}
		if section == "tool.poetry.dependencies" {
			if k, ok := tomlKey(t); ok && k != "python" {
				deps++
			}
		}
	}
	return manifest{file: file, headline: headlineWithName("python package", name), deps: deps, depsCounted: true}
}

// parseCargo reads the crate name and counts direct dependencies of a Cargo.toml via
// a line scan over the [dependencies], [dev-dependencies], and [build-dependencies]
// tables (their per-crate sub-tables counted once each).
func parseCargo(file, data string) manifest {
	name := ""
	deps := 0
	section := ""
	for _, raw := range strings.Split(data, "\n") {
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if h, ok := tomlSection(t); ok {
			section = h
			if cargoDepSubtable(h) {
				deps++
			}
			continue
		}
		if section == "package" && name == "" {
			if v, ok := tomlValue(t, "name"); ok {
				name = v
			}
		}
		if cargoDepSection(section) {
			if _, ok := tomlKey(t); ok {
				deps++
			}
		}
	}
	return manifest{file: file, headline: headlineWithName("rust crate", name), deps: deps, depsCounted: true}
}

func cargoDepSection(s string) bool {
	return s == "dependencies" || s == "dev-dependencies" || s == "build-dependencies"
}

// cargoDepSubtable reports whether s is a per-crate dependency sub-table like
// "dependencies.serde", which declares one dependency.
func cargoDepSubtable(s string) bool {
	for _, p := range []string{"dependencies.", "dev-dependencies.", "build-dependencies."} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// parseGemfile counts the `gem "..."` declarations in a Gemfile.
func parseGemfile(file, data string) manifest {
	n := 0
	for _, raw := range strings.Split(data, "\n") {
		t := strings.TrimSpace(raw)
		if strings.HasPrefix(t, "gem ") || strings.HasPrefix(t, "gem\t") || strings.HasPrefix(t, "gem'") || strings.HasPrefix(t, "gem\"") {
			n++
		}
	}
	return manifest{file: file, headline: "ruby project", deps: n, depsCounted: true}
}

// parsePomXML counts the <dependency> elements of a Maven pom.xml and reads the first
// artifactId as the project name.
func parsePomXML(file, data string) manifest {
	m := manifest{file: file, deps: strings.Count(data, "<dependency>"), depsCounted: true}
	m.headline = headlineWithName("maven project", firstXMLTag(data, "artifactId"))
	return m
}

// parseGradle records a Gradle build file's presence without a dependency count (its
// dependency block has no format-portable count).
func parseGradle(file, _ string) manifest {
	return manifest{file: file, headline: "gradle project"}
}

// headlineWithName appends name to base when non-empty ("node package foo").
func headlineWithName(base, name string) string {
	if name == "" {
		return base
	}
	return base + " " + name
}

// firstXMLTag returns the text of the first <tag>…</tag> in data, or "".
func firstXMLTag(data, tag string) string {
	open := "<" + tag + ">"
	start := strings.Index(data, open)
	if start < 0 {
		return ""
	}
	rest := data[start+len(open):]
	end := strings.Index(rest, "</"+tag+">")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

// tomlSection returns the header name of a "[section]" or "[[array]]" line, false when
// t is not a section header.
func tomlSection(t string) (string, bool) {
	if !strings.HasPrefix(t, "[") {
		return "", false
	}
	end := strings.IndexByte(t, ']')
	if end < 0 {
		return "", false
	}
	inner := strings.TrimPrefix(t[1:end], "[")
	return strings.TrimSpace(inner), true
}

// tomlValue returns the quoted value of a `key = "value"` line, false when t does not
// assign key.
func tomlValue(t, key string) (string, bool) {
	rest, ok := cutKey(t, key)
	if !ok || len(rest) < 2 {
		return "", false
	}
	q := rest[0]
	if q != '"' && q != '\'' {
		return "", false
	}
	if end := strings.IndexByte(rest[1:], q); end >= 0 {
		return rest[1 : 1+end], true
	}
	return "", false
}

// cutKey returns the trimmed right-hand side of a `key = …` assignment (word-boundary
// matched so "namespace" never matches key "name"), false when t does not assign key.
func cutKey(t, key string) (string, bool) {
	if !strings.HasPrefix(t, key) {
		return "", false
	}
	rest := strings.TrimLeft(t[len(key):], " \t")
	if !strings.HasPrefix(rest, "=") {
		return "", false
	}
	return strings.TrimSpace(rest[1:]), true
}

// tomlKey returns the bare key on the left of a `key = …` line, false when t has no
// assignment.
func tomlKey(t string) (string, bool) {
	i := strings.IndexByte(t, '=')
	if i < 0 {
		return "", false
	}
	k := strings.Trim(strings.TrimSpace(t[:i]), `"'`)
	if k == "" {
		return "", false
	}
	return k, true
}

// countQuoted counts the quoted strings in a fragment ("a>=1", "b" → 2), so a
// dependency array's entries can be counted without a TOML parser.
func countQuoted(s string) int {
	n := 0
	inStr := false
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == q {
				inStr = false
			}
			continue
		}
		if c == '"' || c == '\'' {
			inStr = true
			q = c
			n++
		}
	}
	return n
}
