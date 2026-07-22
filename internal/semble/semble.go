// Package semble drives the semble engine: the one-shot CLI argv the facade runs
// for `ccx code search`/`related`, and the MCP tool name plus params the proxy's
// resident semble session calls.
package semble

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/yasyf/cc-context/internal/backend"
	"github.com/yasyf/cc-context/internal/lookpath"
)

// resolve returns the launch prefix for semble's CLI: "semble" when it is on
// PATH, else the uvx invocation that fetches it.
func resolve() (bin string, prefix []string) {
	if lookpath.Find("semble") != "" {
		return "semble", nil
	}
	return "uvx", []string{"--from", "semble[mcp]", "semble"}
}

// CLIArgv translates op into a semble child-process invocation.
func CLIArgv(_ context.Context, op backend.Op, a backend.Args) (bin string, argv []string, err error) {
	bin, argv = resolve()
	switch op {
	case backend.OpSearch:
		argv = append(argv, "search", a.Query)
		if a.Path != "" {
			argv = append(argv, a.Path)
		}
		if a.K > 0 {
			argv = append(argv, "-k", strconv.Itoa(a.K))
		}
		if a.MaxSnippetLines > 0 {
			argv = append(argv, "--max-snippet-lines", strconv.Itoa(a.MaxSnippetLines))
		}
		if a.Kind != "" {
			argv = append(argv, "--content")
			argv = append(argv, strings.Fields(a.Kind)...)
		}
	case backend.OpRelated:
		// semble's find-related CLI takes file and line as two positional args,
		// not a single "file:line" token.
		file, line, lerr := splitLoc(a.Query)
		if lerr != nil {
			return "", nil, lerr
		}
		argv = append(argv, "find-related", file, strconv.Itoa(line))
		if a.Path != "" {
			argv = append(argv, a.Path)
		}
		if a.Kind != "" {
			argv = append(argv, "--content")
			argv = append(argv, strings.Fields(a.Kind)...)
		}
	default:
		return "", nil, fmt.Errorf("semble: unsupported op %q", op)
	}
	return bin, argv, nil
}

// MCPSpec returns the command that launches semble's MCP server over stdio.
// This intentionally does NOT honor on-PATH resolution (unlike the CLI's
// resolve()): the MCP server is exposed only by the semble[mcp] extra, which the
// uvx invocation guarantees. A bare on-PATH `semble` has no MCP-server mode and
// exits on the initialize handshake — do not "unify" this with resolve().
//
// The >=0.5 floor guards index freshness: from 0.5.0 semble revalidates a local
// repo's index on every query (an mtime/file-set check, bounded by a cooldown of
// 3× the last build duration), so a resident session never serves results stale
// against the working tree. The launch takes no positional path — semble's
// argument parsing rejects one — and needs none: the per-call repo param
// (repoOrCwd) selects the repo. The content values index code and docs by
// default.
func MCPSpec(_ context.Context) (cmd string, argv []string, err error) {
	return "uvx", []string{"--from", "semble[mcp]>=0.5", "semble", "--content", "code", "docs"}, nil
}

// MCPTool translates op into a semble MCP tool name and its params.
func MCPTool(op backend.Op, a backend.Args) (tool string, params map[string]any, err error) {
	switch op {
	case backend.OpSearch:
		repo, err := repoOrCwd(a.Path)
		if err != nil {
			return "", nil, err
		}
		return "search", omitEmpty(map[string]any{
			"query":             a.Query,
			"repo":              repo,
			"top_k":             a.K,
			"max_snippet_lines": a.MaxSnippetLines,
		}), nil
	case backend.OpRelated:
		file, line, err := splitLoc(a.Query)
		if err != nil {
			return "", nil, err
		}
		repo, err := repoOrCwd(a.Path)
		if err != nil {
			return "", nil, err
		}
		params := map[string]any{"file_path": file, "line": line}
		if repo != "" {
			params["repo"] = repo
		}
		return "find_related", params, nil
	default:
		return "", nil, fmt.Errorf("semble: unsupported op %q", op)
	}
}

// repoOrCwd returns path when set, else the current working directory. semble's
// MCP tools take no implicit default (unlike its CLI), so the facade must pass
// the project root explicitly or searches return a "no repo specified" notice.
func repoOrCwd(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	return os.Getwd()
}

// splitLoc parses a "file:line" location into its file path and 1-indexed line.
func splitLoc(loc string) (file string, line int, err error) {
	i := strings.LastIndexByte(loc, ':')
	if i < 0 {
		return "", 0, fmt.Errorf("semble: location %q is not file:line", loc)
	}
	line, err = strconv.Atoi(loc[i+1:])
	if err != nil {
		return "", 0, fmt.Errorf("semble: location %q has non-numeric line: %w", loc, err)
	}
	return loc[:i], line, nil
}

// omitEmpty drops zero-valued entries so the params map carries only the fields
// the caller actually set: empty strings, false bools, and zero ints fall out.
func omitEmpty(params map[string]any) map[string]any {
	for k, v := range params {
		switch val := v.(type) {
		case string:
			if val == "" {
				delete(params, k)
			}
		case bool:
			if !val {
				delete(params, k)
			}
		case int:
			if val == 0 {
				delete(params, k)
			}
		}
	}
	return params
}
