package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yasyf/cc-context/internal/gtapi"
	"github.com/yasyf/cc-context/internal/vcstest"
)

const (
	recreateHead = "1111111111111111111111111111111111111111"
	recreateBase = "2222222222222222222222222222222222222222"
)

// recreateGHBody fakes the REST calls pr recreate makes on acme/widgets #7,
// keeping its state and comment in files a test reads back.
const recreateGHBody = `S=$RECREATE_GH
method=GET path= prev= body= labels=
for a in "$@"; do
  case "$a" in repos/*) path=$a ;; body=*) body=${a#body=} ;; labels\[\]=*) labels="$labels${a#*=}," ;; esac
  if [ "$prev" = -X ]; then method=$a; fi
  if [ "$prev" = -f ]; then case "$a" in state=*) state=${a#state=} ;; esac; fi
  prev=$a
done
case "$method $path" in
  "GET repos/acme/widgets/pulls/7")
    read -r st < "$S/state"
    printf '{"number":7,"title":"widgets: a title","body":"The body.","state":"%s","draft":false,"labels":[{"name":"bench"},{"name":"hold"}],"base":{"ref":"main","repo":{"default_branch":"main"}},"head":{"ref":"feature","sha":"` + recreateHead + `"}}' "$st" ;;
  "GET repos/acme/widgets/git/ref/heads/main") echo ` + recreateBase + ` ;;
  "PATCH repos/acme/widgets/pulls/7") printf '%s\n' "$state" > "$S/state" ;;
  "POST repos/acme/widgets/issues/7/comments") printf '%s' "$body" > "$S/comment" ;;
  "POST repos/acme/widgets/issues/100/labels") printf '%s' "$labels" > "$S/labels" ;;
  *) printf 'fake gh: unmatched argv: %s\n' "$*" >&2; exit 2 ;;
esac
`

func recreateRepo(t *testing.T) (*vcstest.Fixture, *gtAPIStub, string) {
	t.Helper()
	f := shipGTRepo(t)
	api := stubGTAPI(t)
	f.Decorate(api.ctx)
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "state"), []byte("open\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("RECREATE_GH", state)
	writeShipExecutable(t, f.ShimBin, "gh", "#!/bin/sh\n"+recreateGHBody)
	return f, api, state
}

func runPRRecreateCmd(t *testing.T, f *vcstest.Fixture, args ...string) (string, error) {
	t.Helper()
	cmd := newVcsPRCmd()
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"recreate", "--repo", "acme/widgets"}, args...))
	err := cmd.ExecuteContext(f.Context())
	return out.String(), err
}

func readRecreateFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestPRRecreateOpensAFreshPullRequestThroughGraphiteAndLinksTheOldOne(t *testing.T) {
	f, api, state := recreateRepo(t)

	out, err := runPRRecreateCmd(t, f, "#7")
	if err != nil {
		t.Fatalf("pr recreate: %v\n%s", err, out)
	}
	if !strings.Contains(out, "recreated #7 as #100 "+gtStubPRURL(100)) {
		t.Errorf("output = %q, want the new pull request named", out)
	}
	if len(api.submits) != 1 {
		t.Fatalf("submits = %d, want one create", len(api.submits))
	}
	entry := api.submits[0].entry
	if entry.Action != gtapi.SubmitCreate || entry.Head != "feature" || entry.HeadSha != recreateHead || entry.Base != "main" || entry.BaseSha != recreateBase {
		t.Errorf("submit entry = %+v, want a create of feature at its head onto main's head", entry)
	}
	if entry.Title == nil || *entry.Title != "widgets: a title" {
		t.Errorf("title = %v, want the old pull request's", entry.Title)
	}
	if entry.Body == nil || *entry.Body != "The body.\n\nReplaces #7 (Graphite lost its stack record)." {
		t.Errorf("body = %v, want the old body and the replacement line", entry.Body)
	}
	if entry.Draft == nil || *entry.Draft {
		t.Errorf("draft = %v, want the old pull request's ready state restated", entry.Draft)
	}
	if got := readRecreateFile(t, state, "state"); got != "closed" {
		t.Errorf("#7 state = %q, want closed", got)
	}
	if got := readRecreateFile(t, state, "comment"); got != "Replaced by #100 (Graphite lost its stack record)." {
		t.Errorf("#7 comment = %q, want a link to #100", got)
	}
	if got := readRecreateFile(t, state, "labels"); got != "bench,hold," {
		t.Errorf("#100 labels = %q, want #7's labels copied", got)
	}
}

func TestPRRecreateReopensTheOldPullRequestWhenGraphiteOpensNone(t *testing.T) {
	f, api, state := recreateRepo(t)
	api.submitErrors["feature"] = "a pull request already exists"

	out, err := runPRRecreateCmd(t, f, "7")
	if err == nil || !strings.Contains(err.Error(), "so it was reopened") {
		t.Fatalf("pr recreate = %v, want the refusal to say #7 was reopened\n%s", err, out)
	}
	if got := readRecreateFile(t, state, "state"); got != "open" {
		t.Errorf("#7 state = %q, want reopened", got)
	}
	if _, err := os.Stat(filepath.Join(state, "comment")); !os.IsNotExist(err) {
		t.Errorf("#7 got a comment (%v), want none without a replacement", err)
	}
}

func TestPRRecreateRefusesAClosedPullRequest(t *testing.T) {
	f, api, state := recreateRepo(t)
	if err := os.WriteFile(filepath.Join(state, "state"), []byte("closed\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := runPRRecreateCmd(t, f, "7"); err == nil || !strings.Contains(err.Error(), "only an open pull request is recreated") {
		t.Fatalf("pr recreate = %v, want a closed pull request refused", err)
	}
	if len(api.submits) != 0 {
		t.Errorf("submits = %d, want none", len(api.submits))
	}
}
