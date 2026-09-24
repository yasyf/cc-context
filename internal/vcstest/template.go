package vcstest

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// baseConfig is the part of a config that decides what the repository looks
// like before any per-test variation is applied: the trunk's name, which tools
// own the working copy, and whether an origin exists. Two fixtures agreeing on
// it get byte-identical trees out of the same tool calls, which is what makes
// the build cacheable.
type baseConfig struct {
	trunk        string
	jj           bool
	gt           bool
	remote       bool
	noOriginHead bool
}

func (c config) base() baseConfig {
	return baseConfig{
		trunk:        c.trunk,
		jj:           c.jj,
		gt:           c.gt,
		remote:       c.remote,
		noOriginHead: c.noOriginHead,
	}
}

// fixtureTemplate is one built base repository, kept for the run and copied
// into each later fixture that wants it. Building it is what a fixture costs —
// `jj git init --colocate` and `gt init` are a second apiece, and the suite
// stands up hundreds — while the tree it produces is the same every time.
type fixtureTemplate struct {
	base string // holds repo/ and, with an origin, remote.git/
	home string
}

var (
	templatesMu sync.Mutex
	templates   = map[baseConfig]*fixtureTemplate{}
	templateDir string
)

// Cleanup removes the templates the run built. A package whose tests stand up
// fixtures calls it from TestMain; without that the templates outlive the
// process as one temporary tree.
func Cleanup() {
	templatesMu.Lock()
	defer templatesMu.Unlock()
	if templateDir != "" {
		_ = os.RemoveAll(templateDir)
		templateDir = ""
	}
	templates = map[baseConfig]*fixtureTemplate{}
}

// templateFor returns the built base repository for cfg, building it on the
// first call. The build runs under the calling test's environment, which the
// caller replaces with the fixture's own once the copy is in place.
func templateFor(t *testing.T, cfg baseConfig, tools []resolvedTool) *fixtureTemplate {
	t.Helper()
	templatesMu.Lock()
	defer templatesMu.Unlock()
	if tmpl, ok := templates[cfg]; ok {
		return tmpl
	}

	if templateDir == "" {
		dir, err := os.MkdirTemp("", "ccx-vcstmpl")
		if err != nil {
			t.Fatalf("create template root: %v", err)
		}
		resolved, err := filepath.EvalSymlinks(dir)
		if err != nil {
			t.Fatalf("resolve template root: %v", err)
		}
		templateDir = resolved
	}

	slot := filepath.Join(templateDir, templateName(cfg))
	tmpl := &fixtureTemplate{base: filepath.Join(slot, "base"), home: filepath.Join(slot, "home")}
	mkdir(t, tmpl.base)
	mkdir(t, tmpl.home)

	applyEnv(t, tmpl.base, tmpl.home, tools)
	buildBase(t, cfg, tools, tmpl.base)
	if cfg.gt {
		// gt's cache refresher outlives gt init and keeps writing under the
		// home it ran with. Copying a template while that is still going
		// walks a file that is gone by the time it is read, so the template
		// is only published once the writing has stopped.
		if !waitQuietTree(tmpl.home) {
			t.Fatalf("template home %s still growing after 5s", tmpl.home)
		}
	}

	templates[cfg] = tmpl
	return tmpl
}

func templateName(cfg baseConfig) string {
	name := cfg.trunk
	for _, part := range []struct {
		on  bool
		tag string
	}{
		{cfg.jj, "jj"},
		{cfg.gt, "gt"},
		{cfg.remote, "remote"},
		{cfg.noOriginHead, "nooriginhead"},
	} {
		if part.on {
			name += "-" + part.tag
		}
	}
	return name
}

// copyTree copies src's contents into dst, which must already exist. It
// carries regular files, directories and symlinks with their permission bits,
// which is everything a git, jj or gt repository is made of.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target) //nolint:gosec // both ends are under temp roots this process made; the template is inert by the time it is copied
		default:
			return copyFile(path, target, info.Mode().Perm())
		}
	})
	if err != nil {
		t.Fatalf("copy %s -> %s: %v", src, dst, err)
	}
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // src is a path the fixture itself wrote under a temp root
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm) //nolint:gosec // dst is under the test's own temp dir
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// buildBase stands up the repository every fixture sharing cfg starts from:
// the working copy, its initial commit, and the bare origin it pushes to.
// The origin is wired as a path relative to the working copy, so the pair
// keeps working wherever the template is copied.
func buildBase(t *testing.T, cfg baseConfig, tools []resolvedTool, base string) {
	t.Helper()
	dir := filepath.Join(base, "repo")
	mkdir(t, dir)

	bin := map[string]string{}
	for _, tool := range tools {
		bin[tool.name] = tool.path
	}
	git := func(args ...string) string { return run(t, dir, bin["git"], args...) }
	jj := func(args ...string) string { return run(t, dir, bin["jj"], args...) }

	git("init", "-q", "-b", cfg.trunk)
	git("config", "user.email", "t@t.t")
	git("config", "user.name", "t")
	writeFile(t, filepath.Join(dir, "f.txt"), "base\n")
	if cfg.jj {
		jj("git", "init", "--colocate")
		jj("commit", "-m", "init")
		jj("bookmark", "create", cfg.trunk, "-r", "@-")
	} else {
		git("add", "f.txt")
		git("commit", "-qm", "init")
	}

	if cfg.remote {
		run(t, base, bin["git"], "init", "-q", "--bare", "--initial-branch="+cfg.trunk, filepath.Join(base, "remote.git"))
		git("remote", "add", "origin", relativeOrigin)
		if cfg.jj {
			jj("git", "push", "--bookmark", cfg.trunk)
		} else {
			git("push", "-q", "origin", cfg.trunk)
		}
		if !cfg.noOriginHead {
			git("symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/"+cfg.trunk)
		}
	}

	if cfg.gt {
		run(t, dir, bin["gt"], "init", "--trunk", cfg.trunk, "--no-interactive")
	}
}

// relativeOrigin is where the bare origin sits from inside the working copy.
// The template is built with it so the pair survives being copied elsewhere;
// pinOrigin then writes the copy's own absolute path, because git resolves a
// relative URL against the process's working directory and a command run from
// a linked worktree would miss it.
const relativeOrigin = "../remote.git"

// pinOrigin rewrites the copied repository's origin from relativeOrigin to
// base's own bare remote. Worktrees share the main repository's config, so the
// one rewrite settles every checkout the fixture goes on to cut.
func pinOrigin(t *testing.T, base string) {
	t.Helper()
	path := filepath.Join(base, "repo", ".git", "config")
	raw, err := os.ReadFile(path) //nolint:gosec // path is the fixture's own config under a temp root
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	want := "url = " + relativeOrigin
	if !strings.Contains(string(raw), want) {
		t.Fatalf("%s names no %s remote to pin", path, relativeOrigin)
	}
	pinned := strings.Replace(string(raw), want, "url = "+filepath.Join(base, "remote.git"), 1)
	if err := os.WriteFile(path, []byte(pinned), 0o600); err != nil { //nolint:gosec // path is the fixture's own config under its t.TempDir
		t.Fatalf("write %s: %v", path, err)
	}
}
