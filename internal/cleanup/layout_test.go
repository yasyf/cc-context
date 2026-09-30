package cleanup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayoutPaths(t *testing.T) {
	l := Layout{Root: "/state"}
	socket, err := l.Socket()
	if err != nil {
		t.Fatalf("Socket() = %v", err)
	}
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"JobsDir", l.JobsDir(), "/state/jobs"},
		{"JobDir", l.JobDir(testID), "/state/jobs/" + testID},
		{"RecordPath", l.RecordPath(testID), "/state/jobs/" + testID + "/job.json"},
		{"Registered", l.Registered(testID), "/state/jobs/" + testID + "/registered"},
		{"Payload", l.Payload(testID), "/state/jobs/" + testID + "/payload"},
		{"Socket", socket, "/state/daemon.sock"},
		{"ServeLockPath", l.ServeLockPath(), "/state/locks/serve.lock"},
		{"StartLockPath", l.StartLockPath(), "/state/locks/start.lock"},
		{"ProgramPath", l.ProgramPath(), "/state/bin/ccx"},
		{"LogPath", l.LogPath(), "/state/daemon.log"},
		{"SwitchPath", l.SwitchPath(), "/state/switch.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestLayoutSocketLimit(t *testing.T) {
	const suffix = "/daemon.sock"
	tests := []struct {
		name  string
		bytes int
		want  string
	}{
		{"longest that fits", 103, ""},
		{"one byte over", 104, "cleanup: socket path is 104 bytes; sun_path fits 103: "},
		{"far over", 200, "cleanup: socket path is 200 bytes; sun_path fits 103: "},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := "/" + strings.Repeat("r", tt.bytes-len(suffix)-1)
			socket, err := Layout{Root: root}.Socket()
			if tt.want == "" {
				if err != nil || socket != root+suffix || len(socket) != tt.bytes {
					t.Fatalf("Socket() = %q, %v; want %q, nil", socket, err, root+suffix)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) || socket != "" {
				t.Fatalf("Socket() = %q, %v; want \"\", error containing %q", socket, err, tt.want)
			}
		})
	}
}

func TestLayoutEnsureCreatesPrivateDirs(t *testing.T) {
	l := Layout{Root: filepath.Join(t.TempDir(), "state")}
	if err := l.Ensure(); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}
	for _, dir := range []string{l.Root, l.JobsDir(), filepath.Join(l.Root, "locks"), filepath.Join(l.Root, "bin")} {
		assertPrivateDir(t, dir)
	}
	if err := l.Ensure(); err != nil {
		t.Fatalf("second Ensure() = %v, want nil", err)
	}
}

func TestLayoutEnsureTightensRoot(t *testing.T) {
	l := Layout{Root: filepath.Join(t.TempDir(), "state")}
	if err := os.Mkdir(l.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.Ensure(); err != nil {
		t.Fatalf("Ensure() = %v", err)
	}
	assertPrivateDir(t, l.Root)
}

func TestLayoutEnsureRejectsRoot(t *testing.T) {
	tests := []struct {
		name string
		root string
	}{
		{"empty", ""},
		{"relative", "state"},
		{"unclean", "/state/../state"},
		{"trailing slash", "/state/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Layout{Root: tt.root}.Ensure()
			want := "cleanup: layout root \"" + tt.root + "\" is not a clean absolute path"
			if err == nil || err.Error() != want {
				t.Fatalf("Ensure() = %v, want %q", err, want)
			}
		})
	}
}

func assertPrivateDir(t *testing.T, dir string) {
	t.Helper()
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Errorf("%s mode = %v, want drwx------", dir, info.Mode())
	}
}
