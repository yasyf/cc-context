package agent

import (
	"bytes"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/yasyf/daemonkit/launchd"

	"github.com/yasyf/cc-context/internal/cleanup"
)

func TestSpec(t *testing.T) {
	root := "/Users/someone/.daemonkit/a/ccx-cleanup"
	want := launchd.Agent{
		Label:         "com.yasyf.ccx-cleanup",
		Program:       root + "/bin/ccx",
		Args:          []string{"vcs", "cleanup", "serve"},
		LogPath:       root + "/daemon.log",
		Env:           map[string]string{"PATH": "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"},
		RestartPolicy: launchd.RestartOnFailure,
		ProcessType:   launchd.ProcessTypeStandard,
	}

	got := Spec(cleanup.Layout{Root: root})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Spec() = %+v, want %+v", got, want)
	}
	plist, err := got.Plist()
	if err != nil {
		t.Fatalf("Plist() error = %v", err)
	}
	for _, fragment := range []string{
		"<string>" + root + "/bin/ccx</string>",
		"<key>RunAtLoad</key>",
		"<key>SuccessfulExit</key>",
		"<string>Standard</string>",
	} {
		if !bytes.Contains(plist, []byte(fragment)) {
			t.Errorf("plist lacks %q:\n%s", fragment, plist)
		}
	}
}

func TestProcessAlive(t *testing.T) {
	child := exec.Command("/usr/bin/true")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		pid  int
		want bool
	}{
		{"this process", os.Getpid(), true},
		{"a reaped child", child.Process.Pid, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := processAlive(tt.pid); got != tt.want {
				t.Errorf("processAlive(%d) = %v, want %v", tt.pid, got, tt.want)
			}
		})
	}
}
