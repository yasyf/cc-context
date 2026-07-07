package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/anchor"
	"github.com/yasyf/cc-context/internal/cli"
)

// runEdit drives the real `code edit` command with the given argv and optional
// stdin, capturing combined output. No engine is touched — edit resolves and
// writes locally.
func runEdit(t *testing.T, stdin string, argv ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	root := cli.NewRootCmd()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"code", "edit"}, argv...))
	err := root.Execute()
	return out.String(), err
}

func TestEditCommandAnchoredReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\ngamma\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	beta := anchor.Of("beta")

	out, err := runEdit(t, "", path, "--at", anchor.Format(2, beta), "--content", "BETA")
	if err != nil {
		t.Fatalf("Execute(edit) error = %v", err)
	}

	if got, _ := os.ReadFile(path); string(got) != "alpha\nBETA\ngamma\n" {
		t.Errorf("file after edit = %q", got)
	}
	want := path + ":" + anchor.Format(2, beta) + " → " + path + ":" + anchor.Format(2, anchor.Of("BETA")) + "\n- beta\n+ BETA\n"
	if out != want {
		t.Errorf("edit output = %q, want %q", out, want)
	}
}

func TestEditCommandContentFromStdin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	out, err := runEdit(t, "X\nY", path, "--at", "2", "--content", "-")
	if err != nil {
		t.Fatalf("Execute(edit) error = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a\nX\nY\nc\n" {
		t.Errorf("file after stdin edit = %q", got)
	}
	want := path + ":" + anchor.Format(2, anchor.Of("b")) + " → " + path + ":" + anchor.FormatRange(2, 3, anchor.Of("X")) + "\n- b\n+ X\n+ Y\n"
	if out != want {
		t.Errorf("edit output = %q, want %q", out, want)
	}
}

func TestEditCommandDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	if _, err := runEdit(t, "", path, "--at", "2", "--delete"); err != nil {
		t.Fatalf("Execute(edit --delete) error = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a\nc\n" {
		t.Errorf("file after delete = %q", got)
	}
}

// TestEditCommandContentWithExplicitDeleteFalse proves an explicit --delete=false
// alongside --content is a no-op flag, not the missing half of the exclusive pair:
// the guard keys off the resolved --delete value, so the content edit applies.
func TestEditCommandContentWithExplicitDeleteFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(path, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	out, err := runEdit(t, "", path, "--at", "2", "--content", "X", "--delete=false")
	if err != nil {
		t.Fatalf("Execute(edit) error = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != "a\nX\nc\n" {
		t.Errorf("file after edit = %q", got)
	}
	want := path + ":" + anchor.Format(2, anchor.Of("b")) + " → " + path + ":" + anchor.Format(2, anchor.Of("X")) + "\n- b\n+ X\n"
	if out != want {
		t.Errorf("edit output = %q, want %q", out, want)
	}
}

func TestEditCommandRequiresExactlyOneOfContentDelete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.txt")
	const content = "a\nb\nc\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	tests := []struct {
		name string
		argv []string
	}{
		{"neither", []string{path, "--at", "2"}},
		{"both", []string{path, "--at", "2", "--content", "X", "--delete"}},
		{"delete=false alone", []string{path, "--at", "2", "--delete=false"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := runEdit(t, "", tt.argv...)
			if err == nil || !strings.Contains(err.Error(), "exactly one") {
				t.Fatalf("Execute(edit) error = %v, want containing %q", err, "exactly one")
			}
			if got, _ := os.ReadFile(path); string(got) != content {
				t.Errorf("file changed on rejected edit: %q", got)
			}
		})
	}
}
