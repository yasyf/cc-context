package deps

import (
	"reflect"
	"testing"

	"github.com/yasyf/cc-context/internal/astgrep"
)

func TestUsesFromOutline(t *testing.T) {
	tests := []struct {
		name  string
		fam   family
		jsonl string
		want  []useItem
	}{
		{
			name:  "go full and std paths",
			fam:   familyGo,
			jsonl: `{"path":"x.go","language":"Go","items":[{"symbolType":"module","name":"context","isImport":true,"range":{"start":{"line":3},"end":{"line":3}}},{"symbolType":"module","name":"github.com/acme/x/internal/foo","isImport":true,"range":{"start":{"line":5},"end":{"line":5}}}]}`,
			want: []useItem{
				{name: "context", line: 4},
				{name: "github.com/acme/x/internal/foo", line: 6},
			},
		},
		{
			name:  "python strips as-alias and keeps from-module",
			fam:   familyPython,
			jsonl: `{"path":"x.py","language":"Python","items":[{"symbolType":"module","name":"numpy as np","isImport":true,"range":{"start":{"line":0},"end":{"line":0}}},{"symbolType":"module","name":"collections","isImport":true,"range":{"start":{"line":1},"end":{"line":1}}},{"symbolType":"module","name":".pkg","isImport":true,"range":{"start":{"line":2},"end":{"line":2}}}]}`,
			want: []useItem{
				{name: "numpy", line: 1},
				{name: "collections", line: 2},
				{name: ".pkg", line: 3},
			},
		},
		{
			name:  "js strips quotes from specifier",
			fam:   familyJS,
			jsonl: `{"path":"x.ts","language":"TypeScript","items":[{"symbolType":"module","name":"'./local'","isImport":true,"range":{"start":{"line":0},"end":{"line":0}}},{"symbolType":"module","name":"'external-pkg'","isImport":true,"range":{"start":{"line":1},"end":{"line":1}}}]}`,
			want: []useItem{
				{name: "./local", line: 1},
				{name: "external-pkg", line: 2},
			},
		},
		{
			name:  "rust passes use path through",
			fam:   familyRust,
			jsonl: `{"path":"x.rs","language":"Rust","items":[{"symbolType":"module","name":"crate::foo::Bar","isImport":true,"range":{"start":{"line":0},"end":{"line":0}}},{"symbolType":"module","name":"std::collections::HashMap","isImport":true,"range":{"start":{"line":1},"end":{"line":1}}}]}`,
			want: []useItem{
				{name: "crate::foo::Bar", line: 1},
				{name: "std::collections::HashMap", line: 2},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, err := astgrep.ParseOutline([]byte(tt.jsonl))
			if err != nil {
				t.Fatalf("ParseOutline: %v", err)
			}
			got := usesFromOutline(tt.fam, files)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("usesFromOutline() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestExtractRegex(t *testing.T) {
	tests := []struct {
		name    string
		fam     family
		content string
		want    []useItem
	}{
		{
			name:    "ruby require and require_relative",
			fam:     familyRuby,
			content: "require 'json'\nrequire_relative \"../lib/util\"\nx = require_notreally\n",
			want: []useItem{
				{name: "json", line: 1},
				{name: "../lib/util", line: 2},
			},
		},
		{
			name:    "c include angle and quote",
			fam:     familyC,
			content: "#include <stdio.h>\n#  include \"local.h\"\nint main(void){}\n",
			want: []useItem{
				{name: "stdio.h", line: 1},
				{name: "local.h", line: 2},
			},
		},
		{
			name:    "csharp using",
			fam:     familyCSharp,
			content: "using System;\nusing static System.Math;\nusing Foo = System.Bar;\n",
			want: []useItem{
				{name: "System", line: 1},
				{name: "System.Math", line: 2},
			},
		},
		{
			name:    "php use and require",
			fam:     familyPHP,
			content: "<?php\nuse App\\Models\\User;\nrequire_once 'vendor/autoload.php';\n",
			want: []useItem{
				{name: `App\Models\User`, line: 2},
				{name: "vendor/autoload.php", line: 3},
			},
		},
		{
			name:    "shell source and dot",
			fam:     familyShell,
			content: "source ./helpers.sh\n. \"$HOME/.env\"\necho hi\n",
			want: []useItem{
				{name: "./helpers.sh", line: 1},
				{name: "$HOME/.env", line: 2},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRegex(tt.fam, tt.content)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractRegex() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestFamilyForExt(t *testing.T) {
	tests := []struct {
		path   string
		want   family
		wantOK bool
	}{
		{"a/b.go", familyGo, true},
		{"a/b.py", familyPython, true},
		{"a/b.PYI", familyPython, true},
		{"a/b.tsx", familyJS, true},
		{"a/b.mjs", familyJS, true},
		{"a/b.rs", familyRust, true},
		{"a/b.rb", familyRuby, true},
		{"a/b.hpp", familyC, true},
		{"a/b.cs", familyCSharp, true},
		{"a/b.php", familyPHP, true},
		{"a/b.bash", familyShell, true},
		{"a/b.txt", 0, false},
		{"README", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, ok := familyForExt(tt.path)
			if ok != tt.wantOK || (ok && got != tt.want) {
				t.Errorf("familyForExt(%q) = (%v, %v), want (%v, %v)", tt.path, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestModuleLine(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"simple", "module github.com/acme/x\n\ngo 1.26\n", "github.com/acme/x"},
		{"leading blank lines", "\n\nmodule example.com/y\n", "example.com/y"},
		{"absent", "go 1.26\nrequire foo v1\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := moduleLine([]byte(tt.data)); got != tt.want {
				t.Errorf("moduleLine() = %q, want %q", got, tt.want)
			}
		})
	}
}
