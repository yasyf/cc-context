package mcpserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/render"
)

func TestServeProfilesServesHeapOnAPrivateSocket(t *testing.T) {
	dir, err := os.MkdirTemp("", "ccxp")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	ctx := render.WithEnv(t.Context(), "CLAUDE_PLUGIN_DATA="+dir)

	stop, err := serveProfiles(ctx)
	if err != nil {
		t.Fatalf("serveProfiles: %v", err)
	}
	sock := filepath.Join(dir, "pprof", strconv.Itoa(os.Getpid())+".sock")
	fi, err := os.Stat(sock)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600", perm)
	}

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://ccx/debug/pprof/heap?debug=1", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET heap: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		t.Fatalf("read heap: %v", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "heap profile") {
		t.Fatalf("GET heap = %d %q, want 200 with a heap profile", resp.StatusCode, body[:min(len(body), 200)])
	}

	stop()
	if _, err := os.Stat(sock); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket after stop: %v, want it removed", err)
	}
}
