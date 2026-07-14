package codeexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"
)

// ServerSpec identifies one discovered MCP server and how to reach it: a stdio
// command line, or a streamable HTTP URL when the listed command is a URL.
// Prefix is the sanitized identifier its reflected host functions carry.
type ServerSpec struct {
	Name    string
	Command string
	Argv    []string
	URL     string
	Prefix  string
}

// Inventory is one discovery probe's surviving servers, the hash that keys the
// tool catalog, and the notes explaining every skipped server.
type Inventory struct {
	Servers []ServerSpec
	Hash    string
	Notes   []string
}

// discoverTimeout bounds the `claude mcp list` probe on a cold or expired
// cache; CCX_EXEC_MCP_TIMEOUT overrides it. Paid at most once per TTL window.
const discoverTimeout = 30 * time.Second

// errBadTimeout marks an unparsable CCX_EXEC_MCP_TIMEOUT: a configuration error
// the engine surfaces loudly, never degrades to a stale-fallback note.
var errBadTimeout = errors.New("invalid CCX_EXEC_MCP_TIMEOUT")

// probeTimeout is the deadline for one probe: CCX_EXEC_MCP_TIMEOUT (a
// time.ParseDuration string) when set, else discoverTimeout. An unparsable
// value wraps errBadTimeout so the engine fails fast rather than falling back.
func probeTimeout() (time.Duration, error) {
	raw := os.Getenv("CCX_EXEC_MCP_TIMEOUT")
	if raw == "" {
		return discoverTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%w %q: %w", errBadTimeout, raw, err)
	}
	return d, nil
}

// serverLine matches one `claude mcp list` health line: name, command-or-URL,
// status. Names may contain colons (plugin:cc-review:cc-review) but never a
// colon-space, so the non-greedy name group stops at the first ": " and the
// non-greedy command group at the first " - ".
var serverLine = regexp.MustCompile(`^(.+?): (.+?) - (.+)$`)

// Discover shells out to `claude mcp list` and returns the connected servers
// that survive the deterministic pre-filters. A probe failure — missing binary,
// timeout, or non-zero exit — returns an error, so the engine can tell it apart
// from a probe that succeeded with zero servers.
func Discover(ctx context.Context) (Inventory, error) {
	if _, err := exec.LookPath("claude"); err != nil {
		return Inventory{}, errors.New("claude not on PATH")
	}
	timeout, err := probeTimeout()
	if err != nil {
		return Inventory{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", "mcp", "list")
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Inventory{}, fmt.Errorf("claude mcp list timed out after %s", timeout)
		}
		return Inventory{}, fmt.Errorf("claude mcp list failed: %w", err)
	}
	return inventoryOf(string(out)), nil
}

// inventoryOf parses `claude mcp list` output and applies the deterministic
// pre-filters, so a filtered server never churns the catalog hash: the
// built-in self-recursion denies (cc-context, plugin:cc-review:*, a ccx
// command), CCX_EXEC_MCP_DENY, and the session-channel heuristic.
// CCX_EXEC_MCP_ALLOW overrides every skip except the built-in denies.
func inventoryOf(out string) Inventory {
	var inv Inventory
	allow := csvSet(os.Getenv("CCX_EXEC_MCP_ALLOW"))
	deny := csvSet(os.Getenv("CCX_EXEC_MCP_DENY"))
	for _, line := range strings.Split(out, "\n") {
		m := serverLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		name, cmdline, status := m[1], strings.TrimSpace(m[2]), m[3]
		if !strings.Contains(status, "Connected") {
			continue
		}
		spec := specOf(name, cmdline)
		if note := skipNote(spec, allow, deny); note != "" {
			inv.Notes = append(inv.Notes, note)
			continue
		}
		inv.Servers = append(inv.Servers, spec)
	}
	slices.SortFunc(inv.Servers, func(a, b ServerSpec) int { return strings.Compare(a.Name, b.Name) })
	inv.Notes = append(inv.Notes, assignPrefixes(inv.Servers)...)
	inv.Hash = inventoryHash(inv.Servers)
	return inv
}

// specOf builds a spec from one health line's fields. A URL command may carry
// a transport suffix like "(HTTP)"; only the URL itself is kept.
func specOf(name, cmdline string) ServerSpec {
	spec := ServerSpec{Name: name}
	fields := strings.Fields(cmdline)
	if strings.HasPrefix(cmdline, "http://") || strings.HasPrefix(cmdline, "https://") {
		spec.URL = fields[0]
		return spec
	}
	spec.Command = fields[0]
	spec.Argv = fields[1:]
	return spec
}

func skipNote(spec ServerSpec, allow, deny map[string]bool) string {
	base := filepath.Base(spec.Command)
	if spec.Name == "cc-context" || strings.HasPrefix(spec.Name, "plugin:cc-review:") || base == "ccx" {
		return fmt.Sprintf("skipped %s: built-in deny — cc-context must not reflect itself back into the sandbox", spec.Name)
	}
	if allow[spec.Name] {
		return ""
	}
	if deny[spec.Name] {
		return fmt.Sprintf("skipped %s: denied by CCX_EXEC_MCP_DENY", spec.Name)
	}
	if strings.Contains(base, "channel") {
		return fmt.Sprintf("skipped %s: command %q looks like a session channel (override with CCX_EXEC_MCP_ALLOW=%s)", spec.Name, base, spec.Name)
	}
	return ""
}

// assignPrefixes gives each survivor its host-function prefix. Servers arrive
// sorted by name, so a collision resolves deterministically: the first keeps
// the base prefix, later ones get a numeric suffix and a note.
func assignPrefixes(servers []ServerSpec) []string {
	var notes []string
	used := make(map[string]bool, len(servers))
	for i := range servers {
		base := prefixOf(servers[i].Name)
		prefix := base
		for n := 2; used[prefix]; n++ {
			prefix = fmt.Sprintf("%s_%d", base, n)
		}
		if prefix != base {
			notes = append(notes, fmt.Sprintf("prefix collision: server %q reflects as %s_* (prefix %s taken)", servers[i].Name, prefix, base))
		}
		used[prefix] = true
		servers[i].Prefix = prefix
	}
	return notes
}

// prefixOf strips a plugin:<mid>: wrapper to the final segment and sanitizes
// it into a Python identifier: plugin:cc-review:cc-review → cc_review.
func prefixOf(name string) string {
	if i := strings.LastIndex(name, ":"); i >= 0 {
		name = name[i+1:]
	}
	return sanitizeIdent(name)
}

var unsafeIdentChar = regexp.MustCompile(`[^a-z0-9_]`)

func sanitizeIdent(s string) string {
	return unsafeIdentChar.ReplaceAllString(strings.ToLower(s), "_")
}

// inventoryHash keys the catalog: sha256 over the sorted name\tcommand lines
// of the survivors, so it is order-independent and moves only when a surviving
// server is added, removed, or relaunched differently.
func inventoryHash(servers []ServerSpec) string {
	lines := make([]string, len(servers))
	for i, s := range servers {
		lines[i] = s.Name + "\t" + s.commandLine()
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

func (s ServerSpec) commandLine() string {
	if s.URL != "" {
		return s.URL
	}
	return strings.Join(append([]string{s.Command}, s.Argv...), " ")
}

func csvSet(csv string) map[string]bool {
	set := map[string]bool{}
	for _, name := range strings.Split(csv, ",") {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = true
		}
	}
	return set
}
