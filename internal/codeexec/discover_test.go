package codeexec

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// realList pins the live `claude mcp list` output observed 2026-07: names may
// contain colons, the plugin line has two spaces before its dash, the check is
// U+2714, and a header precedes the health lines. The linear and broken lines
// extend it with a URL server and a failed server.
const realList = "Checking MCP server health…\n" +
	"\n" +
	"plugin:cc-review:cc-review: /Users/yasyf/.claude/plugins/cache/cc-review/cc-review/0.18.0/scripts/mcp-channel.sh  - ✔ Connected\n" +
	"auggie: /opt/homebrew/bin/auggie --mcp --mcp-auto-workspace - ✔ Connected\n" +
	"railway: railway mcp - ✔ Connected\n" +
	"raindrop: /Users/yasyf/.raindrop/bin/raindrop workshop mcp - ✔ Connected\n" +
	"semble: uvx --from semble[mcp] semble - ✔ Connected\n" +
	"cc-context: ccx mcp - ✔ Connected\n" +
	"linear: https://mcp.linear.app/sse (SSE) - ✔ Connected\n" +
	"broken: npx broken-mcp - ✗ Failed to connect\n"

func clearFilterEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CCX_EXEC_MCP_ALLOW", "")
	t.Setenv("CCX_EXEC_MCP_DENY", "")
}

func TestInventoryOfRealOutput(t *testing.T) {
	clearFilterEnv(t)
	inv := inventoryOf(realList)

	want := []ServerSpec{
		{Name: "auggie", Command: "/opt/homebrew/bin/auggie", Argv: []string{"--mcp", "--mcp-auto-workspace"}, Prefix: "auggie"},
		{Name: "linear", URL: "https://mcp.linear.app/sse", Prefix: "linear"},
		{Name: "railway", Command: "railway", Argv: []string{"mcp"}, Prefix: "railway"},
		{Name: "raindrop", Command: "/Users/yasyf/.raindrop/bin/raindrop", Argv: []string{"workshop", "mcp"}, Prefix: "raindrop"},
		{Name: "semble", Command: "uvx", Argv: []string{"--from", "semble[mcp]", "semble"}, Prefix: "semble"},
	}
	if !reflect.DeepEqual(inv.Servers, want) {
		t.Errorf("Servers = %+v, want %+v", inv.Servers, want)
	}
	for _, sub := range []string{"skipped plugin:cc-review:cc-review", "skipped cc-context"} {
		if !containsSub(inv.Notes, sub) {
			t.Errorf("Notes = %q, missing %q", inv.Notes, sub)
		}
	}
	if inv.Hash == "" {
		t.Error("Hash empty")
	}
}

func TestInventoryFilters(t *testing.T) {
	channelList := "notify: /usr/local/bin/slack-channel.sh --serve - ✔ Connected\n"
	mirrorList := "mirror: /usr/local/bin/ccx mcp - ✔ Connected\n"
	tests := []struct {
		name      string
		list      string
		allow     string
		deny      string
		wantNames []string
		wantNote  string
	}{
		{"deny env skips", realList, "", "railway", []string{"auggie", "linear", "raindrop", "semble"}, "skipped railway: denied by CCX_EXEC_MCP_DENY"},
		{"allow overrides deny", realList, "railway", "railway", []string{"auggie", "linear", "railway", "raindrop", "semble"}, ""},
		{"channel heuristic skips", channelList, "", "", nil, `skipped notify: command "slack-channel.sh" looks like a session channel (override with CCX_EXEC_MCP_ALLOW=notify)`},
		{"allow overrides channel heuristic", channelList, "notify", "", []string{"notify"}, ""},
		{"allow cannot override built-in deny", realList, "cc-context,plugin:cc-review:cc-review", "", []string{"auggie", "linear", "railway", "raindrop", "semble"}, "built-in deny"},
		{"ccx command basename denied", mirrorList, "mirror", "", nil, "skipped mirror: built-in deny"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CCX_EXEC_MCP_ALLOW", tt.allow)
			t.Setenv("CCX_EXEC_MCP_DENY", tt.deny)
			inv := inventoryOf(tt.list)
			var names []string
			for _, s := range inv.Servers {
				names = append(names, s.Name)
			}
			if !reflect.DeepEqual(names, tt.wantNames) {
				t.Errorf("names = %v, want %v", names, tt.wantNames)
			}
			if tt.wantNote != "" && !containsSub(inv.Notes, tt.wantNote) {
				t.Errorf("Notes = %q, missing %q", inv.Notes, tt.wantNote)
			}
		})
	}
}

func TestInventoryHash(t *testing.T) {
	clearFilterEnv(t)
	base := inventoryOf(realList).Hash

	lines := strings.Split(strings.TrimSuffix(realList, "\n"), "\n")
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	reordered := inventoryOf(strings.Join(lines, "\n"))
	if reordered.Hash != base {
		t.Errorf("hash order-dependent: %s != %s", reordered.Hash, base)
	}

	added := inventoryOf(realList + "extra: npx extra-mcp - ✔ Connected\n")
	if added.Hash == base {
		t.Error("hash unchanged after adding a server")
	}

	filteredAdded := inventoryOf(realList + "plugin:cc-review:extra: /x/mcp-channel.sh - ✔ Connected\n")
	if filteredAdded.Hash != base {
		t.Error("filtered server churned the hash")
	}

	relaunched := inventoryOf(strings.Replace(realList, "railway: railway mcp", "railway: railway mcp --beta", 1))
	if relaunched.Hash == base {
		t.Error("hash unchanged after a command change")
	}
}

func TestPrefixes(t *testing.T) {
	clearFilterEnv(t)
	list := "foo-bar: /bin/a serve - ✔ Connected\n" +
		"foo.bar: /bin/b serve - ✔ Connected\n" +
		"plugin:x:tools: /bin/tools-mcp serve - ✔ Connected\n"
	inv := inventoryOf(list)

	prefixes := map[string]string{}
	for _, s := range inv.Servers {
		prefixes[s.Name] = s.Prefix
	}
	want := map[string]string{"foo-bar": "foo_bar", "foo.bar": "foo_bar_2", "plugin:x:tools": "tools"}
	if !reflect.DeepEqual(prefixes, want) {
		t.Errorf("prefixes = %v, want %v", prefixes, want)
	}
	if !containsSub(inv.Notes, "prefix collision") {
		t.Errorf("Notes = %q, missing prefix collision note", inv.Notes)
	}
}

func TestDiscoverClaudeMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Discover(context.Background())
	if err == nil {
		t.Fatal("Discover with no claude = nil, want error")
	}
	if !strings.Contains(err.Error(), "claude not on PATH") {
		t.Errorf("error = %q, want 'claude not on PATH'", err)
	}
}

// writeFakeClaude puts an executable `claude` on PATH running script, so
// Discover shells out to it instead of the real binary.
func writeFakeClaude(t *testing.T, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake shell script is POSIX-only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "claude")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // fake binary must be owner-executable
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDiscoverParsesOutput(t *testing.T) {
	clearFilterEnv(t)
	writeFakeClaude(t, "#!/bin/sh\necho 'fake: /bin/fake-mcp serve - ✔ Connected'\n")
	inv, err := Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	want := []ServerSpec{{Name: "fake", Command: "/bin/fake-mcp", Argv: []string{"serve"}, Prefix: "fake"}}
	if !reflect.DeepEqual(inv.Servers, want) {
		t.Errorf("Servers = %+v, want %+v", inv.Servers, want)
	}
}

func TestDiscoverTimeout(t *testing.T) {
	writeFakeClaude(t, "#!/bin/sh\nsleep 5\n")
	t.Setenv("CCX_EXEC_MCP_TIMEOUT", "100ms")
	_, err := Discover(context.Background())
	if err == nil {
		t.Fatal("Discover on slow claude = nil, want timeout")
	}
	if !strings.Contains(err.Error(), "timed out after 100ms") {
		t.Errorf("error = %q, want 'timed out after 100ms'", err)
	}
}

func TestDiscoverProbeFails(t *testing.T) {
	writeFakeClaude(t, "#!/bin/sh\nexit 1\n")
	_, err := Discover(context.Background())
	if err == nil {
		t.Fatal("Discover on failing claude = nil, want error")
	}
	if !strings.Contains(err.Error(), "claude mcp list failed") {
		t.Errorf("error = %q, want 'claude mcp list failed'", err)
	}
	if strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, must not mention a timeout for a non-timeout failure", err)
	}
}

func TestDiscoverBogusTimeout(t *testing.T) {
	writeFakeClaude(t, "#!/bin/sh\necho hi\n")
	t.Setenv("CCX_EXEC_MCP_TIMEOUT", "not-a-duration")
	_, err := Discover(context.Background())
	if err == nil {
		t.Fatal("Discover with bogus CCX_EXEC_MCP_TIMEOUT = nil, want parse error")
	}
	if !strings.Contains(err.Error(), "CCX_EXEC_MCP_TIMEOUT") {
		t.Errorf("error = %q, want a CCX_EXEC_MCP_TIMEOUT parse error", err)
	}
}

func containsSub(notes []string, sub string) bool {
	for _, n := range notes {
		if strings.Contains(n, sub) {
			return true
		}
	}
	return false
}
