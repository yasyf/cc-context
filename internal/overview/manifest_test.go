package overview

import "testing"

func TestManifestParsers(t *testing.T) {
	tests := []struct {
		name         string
		file         string
		data         string
		parse        func(file, data string) manifest
		wantHeadline string
		wantDeps     int
	}{
		{
			name:  "go.mod block and single-line, indirect skipped",
			file:  "go.mod",
			parse: parseGoMod,
			data: "module github.com/x/y\n\ngo 1.26.5\n\n" +
				"require (\n\tgithub.com/a/b v1.0.0\n\tgithub.com/c/d v2.0.0 // indirect\n)\n\n" +
				"require github.com/e/f v1.2.0\nrequire golang.org/x/tools v0.1.0 // indirect\n",
			wantHeadline: "go module github.com/x/y (go 1.26)",
			wantDeps:     2,
		},
		{
			name:         "go.mod no go directive",
			file:         "go.mod",
			parse:        parseGoMod,
			data:         "module m\n\nrequire foo v1\n",
			wantHeadline: "go module m",
			wantDeps:     1,
		},
		{
			name:         "package.json deps + devDeps",
			file:         "package.json",
			parse:        parsePackageJSON,
			data:         `{"name":"foo","dependencies":{"a":"1","b":"2"},"devDependencies":{"c":"3"}}`,
			wantHeadline: "node package foo",
			wantDeps:     3,
		},
		{
			name:         "package.json no name",
			file:         "package.json",
			parse:        parsePackageJSON,
			data:         `{"dependencies":{"a":"1"}}`,
			wantHeadline: "node package",
			wantDeps:     1,
		},
		{
			name:         "pyproject PEP621 multiline array",
			file:         "pyproject.toml",
			parse:        parsePyproject,
			data:         "[project]\nname = \"myproj\"\ndependencies = [\n  \"click>=8\",\n  \"loguru\",\n]\n",
			wantHeadline: "python package myproj",
			wantDeps:     2,
		},
		{
			name:         "pyproject PEP621 inline array",
			file:         "pyproject.toml",
			parse:        parsePyproject,
			data:         "[project]\nname = \"inl\"\ndependencies = [\"a\", \"b\", \"c\"]\n",
			wantHeadline: "python package inl",
			wantDeps:     3,
		},
		{
			name:         "pyproject poetry table excludes python",
			file:         "pyproject.toml",
			parse:        parsePyproject,
			data:         "[tool.poetry]\nname = \"po\"\n[tool.poetry.dependencies]\npython = \"^3.11\"\nrequests = \"^2.0\"\nrich = \"^13\"\n",
			wantHeadline: "python package po",
			wantDeps:     2,
		},
		{
			name:         "Cargo deps + dev-deps + subtable",
			file:         "Cargo.toml",
			parse:        parseCargo,
			data:         "[package]\nname = \"mycrate\"\n\n[dependencies]\nserde = \"1\"\ntokio = { version = \"1\" }\n\n[dev-dependencies]\ncriterion = \"0.5\"\n\n[dependencies.rustls]\nversion = \"0.23\"\n",
			wantHeadline: "rust crate mycrate",
			wantDeps:     4,
		},
		{
			name:         "composer require + require-dev",
			file:         "composer.json",
			parse:        parseComposer,
			data:         `{"name":"vendor/pkg","require":{"php":">=8","monolog/monolog":"^3"},"require-dev":{"phpunit/phpunit":"^10"}}`,
			wantHeadline: "php package vendor/pkg",
			wantDeps:     3,
		},
		{
			name:         "Gemfile gem lines",
			file:         "Gemfile",
			parse:        parseGemfile,
			data:         "source \"https://rubygems.org\"\ngem \"rails\", \"~> 7\"\ngem 'puma'\n",
			wantHeadline: "ruby project",
			wantDeps:     2,
		},
		{
			name:         "pom.xml dependency count + artifactId",
			file:         "pom.xml",
			parse:        parsePomXML,
			data:         "<project><artifactId>myapp</artifactId><dependencies><dependency>a</dependency><dependency>b</dependency></dependencies></project>",
			wantHeadline: "maven project myapp",
			wantDeps:     2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.parse(tt.file, tt.data)
			if m.file != tt.file {
				t.Errorf("file = %q, want %q", m.file, tt.file)
			}
			if m.headline != tt.wantHeadline {
				t.Errorf("headline = %q, want %q", m.headline, tt.wantHeadline)
			}
			if !m.depsCounted {
				t.Errorf("depsCounted = false, want true")
			}
			if m.deps != tt.wantDeps {
				t.Errorf("deps = %d, want %d", m.deps, tt.wantDeps)
			}
		})
	}
}

func TestParseGradleNoDeps(t *testing.T) {
	m := parseGradle("build.gradle", "dependencies { implementation 'x' }")
	if m.depsCounted {
		t.Errorf("gradle depsCounted = true, want false")
	}
	if m.headline != "gradle project" {
		t.Errorf("headline = %q, want %q", m.headline, "gradle project")
	}
}

func TestManifestsLine(t *testing.T) {
	ms := []manifest{
		{file: "go.mod", deps: 14, depsCounted: true},
		{file: "build.gradle", depsCounted: false},
	}
	want := "manifests: go.mod (14 direct deps) · build.gradle"
	if got := manifestsLine(ms); got != want {
		t.Errorf("manifestsLine = %q, want %q", got, want)
	}
	if got := manifestsLine(nil); got != "" {
		t.Errorf("manifestsLine(nil) = %q, want \"\"", got)
	}
}

func TestProbeManifestsOrder(t *testing.T) {
	root := scaffold(t, map[string]string{
		"Cargo.toml": "[package]\nname = \"c\"\n",
		"go.mod":     "module m\ngo 1.26\n",
	})
	ms := probeManifests(root)
	if len(ms) != 2 {
		t.Fatalf("probeManifests returned %d, want 2", len(ms))
	}
	if ms[0].file != "go.mod" {
		t.Errorf("primary manifest = %q, want go.mod", ms[0].file)
	}
}
