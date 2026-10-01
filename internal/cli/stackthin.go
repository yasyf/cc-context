package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yasyf/cc-context/internal/cache"
	"github.com/yasyf/cc-context/internal/gtmeta"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

const (
	stackNewEnv         = "CCX_STACK_NEW"
	thinDefaultDepth    = 256
	thinDefaultMaxDepth = 4096
	thinRemote          = "origin"
	thinAdoptedPrefix   = "refs/ccx/thin-adopted/"
)

var (
	thinStoreConfig    = []string{"feature.experimental=true", "feature.manyFiles=true", "pack.threads=2"}
	thinMirroredConfig = []string{"user.name", "user.email", "user.signingkey", "commit.gpgsign", nogtKey}
	thinMetadataDirs   = []string{".claude", ".agents"}
	thinDefaultPorts   = map[string]string{"ssh": "22", "git+ssh": "22", "ssh+git": "22", "git": "9418", "http": "80", "https": "443"}
)

type stackStorage int

const (
	storageCaller stackStorage = iota
	storageThin
	storageFullHistory
)

func stackStorageOf(ctx context.Context, o stackNewOpts) (stackStorage, error) {
	switch {
	case o.thin:
		return storageThin, nil
	case o.fullHistory:
		return storageFullHistory, nil
	}
	switch v := render.Getenv(ctx, stackNewEnv); v {
	case "", "full":
		return storageCaller, nil
	case "thin":
		return storageThin, nil
	default:
		return 0, fmt.Errorf("stack new: %s=%q is neither thin nor full", stackNewEnv, v)
	}
}

type thinSource struct {
	url       string
	canonical string
	trunk     string
}

func thinSourceOf(ctx context.Context, src lane) (thinSource, error) {
	remote, err := vcs.GitRemoteFor(ctx, src.dir(), "HEAD")
	if err != nil {
		return thinSource{}, fmt.Errorf("stack new: %w", err)
	}
	out, err := render.RunCLI(ctx, src.dir(), "git", []string{"config", "--get", "remote." + remote + ".url"})
	if err != nil {
		return thinSource{}, fmt.Errorf("stack new: read the url of %s: %w", remote, err)
	}
	s := thinSource{url: strings.TrimSpace(out)}
	if s.canonical, err = thinCanonicalRemote(s.url); err != nil {
		return thinSource{}, fmt.Errorf("stack new: %w", err)
	}
	if src.gt {
		s.trunk, err = gtmeta.ReadTrunk(src.checkout.CommonDir)
	} else {
		var tr vcs.Trunk
		if tr, err = vcs.ResolveTrunk(ctx, src.dir(), remote); err == nil {
			s.trunk = tr.Name()
		}
	}
	if err != nil {
		return thinSource{}, fmt.Errorf("stack new: %w", err)
	}
	return s, nil
}

func thinCanonicalRemote(raw string) (string, error) {
	switch {
	case filepath.IsAbs(raw):
		return filepath.Clean(raw), nil
	case strings.Contains(raw, "://"):
		u, err := url.Parse(raw)
		if err != nil {
			return "", errors.New("the remote URL does not parse")
		}
		if u.Scheme == "file" {
			if u.Host != "" && u.Host != "localhost" || !filepath.IsAbs(u.Path) {
				return "", errors.New("the file remote names no local absolute path")
			}
			return filepath.Clean(u.Path), nil
		}
		host := strings.ToLower(u.Hostname())
		if port := u.Port(); port != "" && port != thinDefaultPorts[u.Scheme] {
			host = net.JoinHostPort(host, port)
		}
		return thinHostedRemote(host, u.Path)
	default:
		host, p, ok := strings.Cut(raw, ":")
		if !ok {
			return "", errors.New("the remote is neither a URL, an scp-style address, nor an absolute path")
		}
		if _, after, found := strings.Cut(host, "@"); found {
			host = after
		}
		return thinHostedRemote(strings.ToLower(host), p)
	}
}

func thinHostedRemote(host, p string) (string, error) {
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	if host == "" || p == "" {
		return "", errors.New("the remote names no host and repository")
	}
	return host + "/" + p, nil
}

func thinStoresRoot(ctx context.Context) (string, error) {
	home, err := render.Home(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	if home, err = filepath.EvalSymlinks(home); err != nil {
		return "", fmt.Errorf("canonicalize home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "stores"), nil
}

func thinStorePath(ctx context.Context, mainRoot, canonical string) (string, error) {
	if mainRoot == "" {
		return "", errors.New("stack new: no main working copy to name a thin store after")
	}
	root, err := thinStoresRoot(ctx)
	if err != nil {
		return "", fmt.Errorf("stack new: %w", err)
	}
	sum := sha256.Sum256([]byte(canonical))
	return filepath.Join(root, hex.EncodeToString(sum[:])[:12], filepath.Base(mainRoot)), nil
}

func thinIsStore(ctx context.Context, c vcs.Checkout) (bool, error) {
	if c.CommonDir == "" {
		return false, nil
	}
	root, err := thinStoresRoot(ctx)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(root, c.CommonDir)
	if err != nil {
		return false, err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[0] == ".." || parts[2] != ".git" {
		return false, nil
	}
	store := filepath.Dir(c.CommonDir)
	out, code, stderr, err := render.RunCLIExitCode(ctx, render.Dir(store), "git", []string{"config", "--get", "remote." + thinRemote + ".url"})
	switch {
	case err != nil:
		return false, err
	case code == 1:
		return false, fmt.Errorf("%s sits where ccx keeps thin stores but has no %s remote, so ccx will not treat it as an ordinary checkout — restore remote.%s.url or move it out of %s", store, thinRemote, thinRemote, root)
	case code != 0:
		return false, fmt.Errorf("read the url of %s in %s: %s", thinRemote, store, strings.TrimSpace(stderr))
	}
	canonical, err := thinCanonicalRemote(strings.TrimSpace(out))
	if err != nil {
		return false, fmt.Errorf("%s sits where ccx keeps thin stores but its %s remote is unreadable: %w", store, thinRemote, err)
	}
	want, err := thinStorePath(ctx, store, canonical)
	if err != nil {
		return false, err
	}
	if want != store {
		return false, fmt.Errorf("%s sits where ccx keeps thin stores but its %s remote (%s) derives %s, so ccx will not treat it as an ordinary checkout — restore remote.%s.url or move it out of %s", store, thinRemote, canonical, want, thinRemote, root)
	}
	return true, nil
}

func thinStoreOf(ctx context.Context, prefix string, src lane) (lane, bool, error) {
	remote, err := vcs.GitRemoteFor(ctx, src.dir(), "HEAD")
	if err != nil {
		return lane{}, false, fmt.Errorf("%s: %w", prefix, err)
	}
	canonical, ok, err := thinRemoteOf(ctx, src.dir(), remote)
	if err != nil || !ok {
		return lane{}, false, err
	}
	root, err := thinStorePath(ctx, src.checkout.MainRoot, canonical)
	if err != nil {
		return lane{}, false, err
	}
	if _, err := os.Lstat(root); errors.Is(err, fs.ErrNotExist) {
		return lane{}, false, nil
	} else if err != nil {
		return lane{}, false, fmt.Errorf("%s: %w", prefix, err)
	}
	stored, ok, err := thinRemoteOf(ctx, render.Dir(root), thinRemote)
	if err != nil {
		return lane{}, false, err
	}
	if !ok || stored != canonical {
		return lane{}, false, fmt.Errorf("%s: %s is not a thin store of %s", prefix, root, canonical)
	}
	store, err := resolveLane(ctx, prefix, root, true)
	return store, err == nil, err
}

func thinRemoteOf(ctx context.Context, dir render.Dir, remote string) (string, bool, error) {
	out, code, stderr, err := render.RunCLIExitCode(ctx, dir, "git", []string{"config", "--get", "remote." + remote + ".url"})
	switch {
	case err != nil:
		return "", false, err
	case code == 1:
		return "", false, nil
	case code != 0:
		return "", false, fmt.Errorf("read the url of %s: %s", remote, strings.TrimSpace(stderr))
	}
	canonical, err := thinCanonicalRemote(strings.TrimSpace(out))
	return canonical, err == nil, nil
}

func thinEnsureStore(ctx context.Context, errW io.Writer, src lane, s thinSource, o stackNewOpts) (lane, bool, error) {
	root, err := thinStorePath(ctx, src.checkout.MainRoot, s.canonical)
	if err != nil {
		return lane{}, false, err
	}
	created := false
	switch _, err := os.Lstat(root); {
	case err == nil:
		if o.depthSet {
			return lane{}, false, fmt.Errorf("stack new: --depth applies only when a thin store is created, and %s already exists", root)
		}
	case errors.Is(err, fs.ErrNotExist):
		if created, err = thinCreateStore(ctx, src, s, root, o.depth); err != nil {
			return lane{}, false, err
		}
	default:
		return lane{}, false, fmt.Errorf("stack new: %w", err)
	}
	if err := thinVerifyStore(ctx, root, s); err != nil {
		return lane{}, false, err
	}
	ck, err := vcs.ResolveCheckout(root)
	if err != nil {
		return lane{}, false, fmt.Errorf("stack new: %w", err)
	}
	store := lane{kind: vcs.Git, root: ck.Root, checkout: ck, gt: src.gt, note: src.note, repo: src.repo, verdict: src.verdict}
	if src.gt {
		if err := thinInitGraphite(ctx, errW, store, s.trunk); err != nil {
			return lane{}, false, err
		}
	}
	if err := thinSeedCaches(ctx, src.root, store.root); err != nil {
		return lane{}, false, err
	}
	return store, created, nil
}

func thinCreateStore(ctx context.Context, src lane, s thinSource, root string, depth int) (bool, error) {
	parent := filepath.Dir(root)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return false, fmt.Errorf("stack new: %w", err)
	}
	stage, err := os.MkdirTemp(parent, "."+filepath.Base(root)+"-")
	if err != nil {
		return false, fmt.Errorf("stack new: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	staged := filepath.Join(stage, filepath.Base(root))
	argv := []string{"clone", "--no-local", "--no-checkout", "--depth=" + strconv.Itoa(depth), "--filter=blob:none", "--no-tags", "--single-branch", "--branch", s.trunk, "--ref-format=files"}
	for _, kv := range thinStoreConfig {
		argv = append(argv, "-c", kv)
	}
	if _, err := render.RunCLI(ctx, render.Dir(stage), "git", append(argv, s.url, staged)); err != nil {
		return false, fmt.Errorf("stack new: clone a thin store of %s: %w", s.canonical, err)
	}
	if err := thinVerifyStore(ctx, staged, s); err != nil {
		return false, err
	}
	if err := thinPrepareStore(ctx, src, s, render.Dir(staged)); err != nil {
		return false, err
	}
	if err := os.Rename(staged, root); err != nil {
		if _, statErr := os.Lstat(root); statErr == nil {
			return false, nil
		}
		return false, fmt.Errorf("stack new: install the thin store at %s: %w", root, err)
	}
	return true, nil
}

func thinPrepareStore(ctx context.Context, src lane, s thinSource, dir render.Dir) error {
	if _, err := render.RunCLI(ctx, dir, "git", []string{"remote", "set-head", thinRemote, s.trunk}); err != nil {
		return fmt.Errorf("stack new: set %s/HEAD to %s: %w", thinRemote, s.trunk, err)
	}
	merge, err := render.RunCLI(ctx, dir, "git", []string{"config", "--get", "branch." + s.trunk + ".merge"})
	if err != nil {
		return fmt.Errorf("stack new: the thin store's %s tracks nothing: %w", s.trunk, err)
	}
	if strings.TrimSpace(merge) != gtRestackRef(s.trunk) {
		return fmt.Errorf("stack new: the thin store's %s tracks %s, not %s", s.trunk, strings.TrimSpace(merge), gtRestackRef(s.trunk))
	}
	if _, err := render.RunCLI(ctx, dir, "git", []string{"-c", "core.hooksPath=/dev/null", "sparse-checkout", "set", "--cone", "--no-sparse-index"}); err != nil {
		return fmt.Errorf("stack new: make the thin store sparse: %w", err)
	}
	head, err := stackRevParse(ctx, dir, "HEAD")
	if err != nil {
		return err
	}
	if err := stackPopulateSparse(ctx, dir, "stack new", head); err != nil {
		return err
	}
	for _, key := range thinMirroredConfig {
		out, code, stderr, err := render.RunCLIExitCode(ctx, src.dir(), "git", []string{"config", "--local", "--get", key})
		switch {
		case err != nil:
			return err
		case code == 1:
			continue
		case code != 0:
			return fmt.Errorf("stack new: read %s: %s", key, strings.TrimSpace(stderr))
		}
		if _, err := render.RunCLI(ctx, dir, "git", []string{"config", key, strings.TrimSpace(out)}); err != nil {
			return fmt.Errorf("stack new: set %s in the thin store: %w", key, err)
		}
	}
	return nil
}

func thinVerifyStore(ctx context.Context, root string, s thinSource) error {
	out, err := render.RunCLI(ctx, render.Dir(root), "git", []string{"config", "--get-regexp", `^remote\.` + thinRemote + `\.`})
	if err != nil {
		return fmt.Errorf("stack new: read %s's remote: %w", root, err)
	}
	got := map[string][]string{}
	for line := range strings.Lines(out) {
		key, value, _ := strings.Cut(strings.TrimSpace(line), " ")
		key = strings.TrimPrefix(key, "remote."+thinRemote+".")
		got[key] = append(got[key], value)
	}
	want := map[string][]string{
		"promisor":           {"true"},
		"partialclonefilter": {"blob:none"},
		"tagopt":             {"--no-tags"},
		"fetch":              {"+" + gtRestackRef(s.trunk) + ":refs/remotes/" + thinRemote + "/" + s.trunk},
	}
	for _, key := range slices.Sorted(maps.Keys(want)) {
		if !slices.Equal(got[key], want[key]) {
			return fmt.Errorf("stack new: %s is not a thin store of %s: remote.%s.%s is %q, want %q", root, s.canonical, thinRemote, key, got[key], want[key])
		}
	}
	canonical := ""
	if urls := got["url"]; len(urls) == 1 {
		canonical, _ = thinCanonicalRemote(urls[0])
	}
	if canonical != s.canonical {
		return fmt.Errorf("stack new: %s is not a thin store of %s: its %s remote reads as %q", root, s.canonical, thinRemote, canonical)
	}
	return nil
}

func thinInitGraphite(ctx context.Context, errW io.Writer, store lane, trunk string) error {
	present, err := vcs.GraphiteRepo(store.checkout)
	if err != nil || present {
		return err
	}
	r, runErr := gtRun(ctx, store.dir(), []string{"init", "--trunk", trunk, "--no-interactive"}, errW)
	if err := gtReport(ctx, errW, r); err != nil {
		return err
	}
	if runErr != nil {
		return fmt.Errorf("stack new: gt init --trunk %s in %s: %w", trunk, store.root, runErr)
	}
	return nil
}

func thinSeedCaches(ctx context.Context, srcRoot, storeRoot string) error {
	for _, at := range []func(context.Context, string) (string, error){vcs.RepoCachePath, gtCachePath} {
		from, err := at(ctx, srcRoot)
		if err != nil {
			return err
		}
		to, err := at(ctx, storeRoot)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(to); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		data, err := os.ReadFile(from) //nolint:gosec // from is rooted at the cache dir and keyed by sha256 hex
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := cache.Store(to, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func thinAdoptedRef(branch string) string {
	sum := sha256.Sum256([]byte(branch))
	return thinAdoptedPrefix + hex.EncodeToString(sum[:])
}

func thinAdopted(ctx context.Context, store lane, branch string) (bool, error) {
	return gitRefExists(ctx, store.dir(), "stack new", thinAdoptedRef(branch))
}

func thinAdopt(ctx context.Context, src, store lane, receipt *stackPublication, trunk string, o stackNewOpts) (int, error) {
	if receipt.Parent != trunk {
		return 0, fmt.Errorf("stack new: %s was published onto %s, not %s, and only parents published onto trunk are brought into a thin store — create the lane with --full-history", receipt.Branch, receipt.Parent, trunk)
	}
	dir := store.dir()
	reachable, err := thinReachable(ctx, dir, receipt.Base, "refs/remotes/"+thinRemote+"/"+trunk)
	if err != nil {
		return 0, err
	}
	deepened := 0
	if !reachable {
		if !o.deepen {
			return 0, fmt.Errorf("stack new: %s's published base %s lies beyond the history of thin store %s; nothing changed — pass --deepen to fetch trunk history down to it, at most --max-depth %d commits, or create the lane with --full-history", receipt.Branch, shortOID(receipt.Base), store.root, o.maxDepth)
		}
		if deepened, err = thinDeepen(ctx, store, trunk, receipt.Base, o); err != nil {
			return deepened, err
		}
	}
	remoteRef := "refs/remotes/" + thinRemote + "/" + receipt.Branch
	if err := gitFetch(ctx, dir, "--no-tags", "--no-write-fetch-head", thinRemote, "+"+gtRestackRef(receipt.Branch)+":"+remoteRef); err != nil {
		return deepened, fmt.Errorf("stack new: fetch %s into the thin store: %w", receipt.Branch, err)
	}
	fetched, err := stackRevParse(ctx, dir, remoteRef)
	if err != nil {
		return deepened, err
	}
	if fetched != receipt.Head {
		return deepened, fmt.Errorf("stack new: %s is at %s on %s, not at its published %s; publish it again before creating a child", receipt.Branch, shortOID(fetched), thinRemote, shortOID(receipt.Head))
	}
	complete, err := thinComplete(ctx, store, receipt.Base, receipt.Head)
	if err != nil {
		return deepened, err
	}
	if !complete {
		return deepened, fmt.Errorf("stack new: %s's commits above %s are cut by thin store %s's shallow boundary; nothing was recorded — create the lane with --full-history", receipt.Branch, shortOID(receipt.Base), store.root)
	}
	if err := thinRecordAdopted(ctx, store, trunk, receipt); err != nil {
		return deepened, err
	}
	return deepened, thinCopyReceipt(ctx, src, store, receipt)
}

func thinDeepen(ctx context.Context, store lane, trunk, base string, o stackNewOpts) (int, error) {
	total := 0
	for step := o.depth; ; step *= 2 {
		if total >= o.maxDepth {
			return total, fmt.Errorf("stack new: %s is still beyond thin store %s's history after deepening trunk by %d commits, the --max-depth cap; the store only grew — raise --max-depth or create the lane with --full-history", shortOID(base), store.root, total)
		}
		step = min(step, o.maxDepth-total)
		if err := thinFetchDeepen(ctx, store, trunk, step); err != nil {
			return total, err
		}
		total += step
		reachable, err := thinReachable(ctx, store.dir(), base, "refs/remotes/"+thinRemote+"/"+trunk)
		if err != nil || reachable {
			return total, err
		}
	}
}

func thinFetchDeepen(ctx context.Context, store lane, trunk string, step int) error {
	refspec := "+" + gtRestackRef(trunk) + ":refs/remotes/" + thinRemote + "/" + trunk
	lock := filepath.Join(store.checkout.CommonDir, "shallow.lock")
	delay := 100 * time.Millisecond
	for attempt := 1; ; attempt++ {
		err := gitFetch(ctx, store.dir(), "--no-tags", "--no-write-fetch-head", "--deepen="+strconv.Itoa(step), thinRemote, refspec)
		if err == nil {
			return nil
		}
		if _, statErr := os.Lstat(lock); statErr != nil || attempt == gitFetchAttempts {
			return fmt.Errorf("stack new: deepen %s by %d commits in %s: %w", trunk, step, store.root, err)
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(delay):
		}
		delay *= 2
	}
}

func thinReachable(ctx context.Context, dir render.Dir, commit, rev string) (bool, error) {
	_, code, stderr, err := render.RunCLIExitCodeEnv(ctx, dir, "git", []string{"cat-file", "-e", commit}, gitRecordedHistoryEnv)
	switch {
	case err != nil:
		return false, err
	case code == 1:
		return false, nil
	case code != 0:
		return false, fmt.Errorf("git cat-file -e %s: exit %d: %s", shortOID(commit), code, strings.TrimSpace(stderr))
	}
	_, code, stderr, err = render.RunCLIExitCodeEnv(ctx, dir, "git", []string{"merge-base", "--is-ancestor", commit, rev}, gitRecordedHistoryEnv)
	switch {
	case err != nil:
		return false, err
	case code == 0:
		return true, nil
	case code == 1:
		return false, nil
	default:
		return false, fmt.Errorf("git merge-base --is-ancestor %s %s: exit %d: %s", shortOID(commit), rev, code, strings.TrimSpace(stderr))
	}
}

func thinComplete(ctx context.Context, store lane, base, head string) (bool, error) {
	reachable, err := thinReachable(ctx, store.dir(), base, head)
	if err != nil || !reachable {
		return false, err
	}
	shallow, err := stackShallowSet(store.checkout.CommonDir)
	if err != nil {
		return false, err
	}
	return stackRangeWhole(ctx, store.dir(), shallow, head, "^"+base)
}

func thinRecordAdopted(ctx context.Context, store lane, trunk string, receipt *stackPublication) error {
	dir := store.dir()
	ref, mark := gtRestackRef(receipt.Branch), thinAdoptedRef(receipt.Branch)
	present, err := gitRefExists(ctx, dir, "stack new", ref)
	if err != nil {
		return err
	}
	tx := "start\ncreate " + ref + " " + receipt.Head + "\nupdate " + mark + " " + receipt.Head + "\ncommit\n"
	if present {
		adopted, err := thinAdopted(ctx, store, receipt.Branch)
		if err != nil {
			return err
		}
		if !adopted {
			current, err := stackRevParse(ctx, dir, ref)
			if err != nil {
				return err
			}
			return fmt.Errorf("stack new: thin store %s has its own branch %s at %s; nothing changed — cut the lane from a checkout of the store, or create it with --full-history", store.root, receipt.Branch, shortOID(current))
		}
		marked, err := stackRevParse(ctx, dir, mark)
		if err != nil {
			return err
		}
		tx = "start\nupdate " + ref + " " + receipt.Head + " " + marked + "\nupdate " + mark + " " + receipt.Head + " " + marked + "\ncommit\n"
	}
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "--stdin"}, []byte(tx)); err != nil {
		return fmt.Errorf("stack new: record %s at %s in the thin store: %w", receipt.Branch, shortOID(receipt.Head), err)
	}
	return gtmeta.AdoptFrozen(ctx, store.checkout.CommonDir, receipt.Branch, trunk, receipt.Base, receipt.Head)
}

func thinCopyReceipt(ctx context.Context, src, store lane, receipt *stackPublication) error {
	blob, err := render.RunCLI(ctx, src.dir(), "git", []string{"cat-file", "blob", receipt.OID})
	if err != nil {
		return fmt.Errorf("stack new: read %s's receipt: %w", receipt.Branch, err)
	}
	out, err := render.RunCLIStdin(ctx, store.dir(), "git", []string{"hash-object", "-w", "--stdin"}, []byte(blob))
	if err != nil {
		return fmt.Errorf("stack new: copy %s's receipt: %w", receipt.Branch, err)
	}
	if oid := strings.TrimSpace(out); oid != receipt.OID {
		return fmt.Errorf("stack new: %s's receipt copied as %s, not %s", receipt.Branch, oid, receipt.OID)
	}
	if _, err := render.RunCLI(ctx, store.dir(), "git", []string{"update-ref", stackPublicationRef(receipt.Branch, "receipt"), receipt.OID}); err != nil {
		return fmt.Errorf("stack new: record %s's receipt: %w", receipt.Branch, err)
	}
	return nil
}

func stackShallowSet(commonDir string) (map[string]bool, error) {
	data, err := os.ReadFile(filepath.Join(commonDir, "shallow")) //nolint:gosec // the repository's own shallow file
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, sha := range strings.Fields(string(data)) {
		set[sha] = true
	}
	return set, nil
}

func stackRangeWhole(ctx context.Context, dir render.Dir, shallow map[string]bool, revs ...string) (bool, error) {
	out, code, stderr, err := render.RunCLIExitCodeEnv(ctx, dir, "git", append([]string{"rev-list"}, revs...), gitRecordedHistoryEnv)
	if err != nil {
		return false, err
	}
	if code != 0 {
		return false, fmt.Errorf("git rev-list %s: exit %d: %s", strings.Join(revs, " "), code, strings.TrimSpace(stderr))
	}
	for _, sha := range strings.Fields(out) {
		if shallow[sha] {
			return false, nil
		}
	}
	return true, nil
}

func stackRequireHistory(ctx context.Context, dir render.Dir, prefix, remote, trunk string, heads map[string]string) error {
	ck, err := vcs.ResolveCheckout(string(dir))
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	shallow, err := stackShallowSet(ck.CommonDir)
	if err != nil || shallow == nil {
		return err
	}
	trunkRef := "refs/remotes/" + remote + "/" + trunk
	for _, name := range slices.Sorted(maps.Keys(heads)) {
		if heads[name] == "" {
			continue
		}
		_, code, stderr, err := render.RunCLIExitCodeEnv(ctx, dir, "git", []string{"merge-base", heads[name], trunkRef}, gitRecordedHistoryEnv)
		if err != nil {
			return err
		}
		if code > 1 {
			return fmt.Errorf("%s: git merge-base %s %s: exit %d: %s", prefix, heads[name], trunkRef, code, strings.TrimSpace(stderr))
		}
		whole := code == 0
		if whole {
			if whole, err = stackRangeWhole(ctx, dir, shallow, heads[name], "^"+trunkRef); err != nil {
				return err
			}
		}
		if !whole {
			return fmt.Errorf("%s: %s's history in %s stops at its shallow boundary before it meets %s/%s; nothing moved — fetch the missing history explicitly with git -C %s fetch --deepen=<commits> %s %s, then run this again", prefix, name, ck.MainRoot, remote, trunk, ck.MainRoot, remote, trunk)
		}
	}
	return nil
}

func thinRecordPush(ctx context.Context, dir render.Dir, remote string, pushed map[string]string) error {
	if len(pushed) == 0 {
		return nil
	}
	ck, store, err := thinDirIsStore(ctx, dir)
	if err != nil || !store {
		return err
	}
	var tx strings.Builder
	tx.WriteString("start\n")
	for _, branch := range slices.Sorted(maps.Keys(pushed)) {
		fmt.Fprintf(&tx, "update refs/remotes/%s/%s %s\n", remote, branch, pushed[branch])
	}
	tx.WriteString("commit\n")
	if _, err := render.RunCLIStdin(ctx, dir, "git", []string{"update-ref", "-m", "update by push", "--stdin"}, []byte(tx.String())); err != nil {
		return fmt.Errorf("record the push in %s's remote-tracking refs: %w", ck.Root, err)
	}
	return nil
}

func thinDirIsStore(ctx context.Context, dir render.Dir) (vcs.Checkout, bool, error) {
	ck, err := vcs.ResolveCheckout(string(dir))
	if err != nil {
		return vcs.Checkout{}, false, err
	}
	store, err := thinIsStore(ctx, ck)
	return ck, store, err
}

func thinRecordBranchPush(ctx context.Context, dir render.Dir, remote, branch string) error {
	if _, store, err := thinDirIsStore(ctx, dir); err != nil || !store {
		return err
	}
	head, err := stackRevParse(ctx, dir, gtRestackRef(branch))
	if err != nil {
		return err
	}
	return thinRecordPush(ctx, dir, remote, map[string]string{branch: head})
}

func thinRefuseAdoptedPush(ctx context.Context, dir render.Dir, prefix string, branches []string) error {
	if len(branches) == 0 {
		return nil
	}
	ck, store, err := thinDirIsStore(ctx, dir)
	if err != nil {
		return fmt.Errorf("%s: %w", prefix, err)
	}
	if !store {
		return nil
	}
	names := slices.Sorted(slices.Values(branches))
	var marks strings.Builder
	for _, name := range names {
		marks.WriteString(thinAdoptedRef(name) + "\n")
	}
	out, err := render.RunCLIStdin(ctx, dir, "git", []string{"cat-file", "--batch-check"}, []byte(marks.String()))
	if err != nil {
		return fmt.Errorf("%s: look up the adoption marks in %s: %w", prefix, ck.Root, err)
	}
	rows := slices.Collect(strings.Lines(out))
	if len(rows) != len(names) {
		return fmt.Errorf("%s: git cat-file --batch-check answered %d rows for %d adoption marks in %s", prefix, len(rows), len(names), ck.Root)
	}
	for i, branch := range names {
		fields := strings.Fields(rows[i])
		switch {
		case len(fields) == 2 && fields[0] == thinAdoptedRef(branch) && fields[1] == "missing":
		case len(fields) == 3 && thinIsOID(fields[0]):
			return fmt.Errorf("%s: %s was adopted into thin store %s from the source checkout that owns it, and ccx never pushes an adopted parent from the store; nothing was pushed — publish %s from that checkout", prefix, branch, ck.Root, branch)
		default:
			return fmt.Errorf("%s: git cat-file --batch-check answered %q for %s's adoption mark in %s", prefix, strings.TrimSpace(rows[i]), branch, ck.Root)
		}
	}
	return nil
}

func thinIsOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
