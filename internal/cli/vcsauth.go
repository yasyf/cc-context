package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yasyf/cc-context/internal/ghapi"
	"github.com/yasyf/cc-context/internal/render"
	"github.com/yasyf/cc-context/internal/vcs"
)

func newVcsAuthCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Report which GitHub identity ccx authenticates as",
		Args:  cobra.NoArgs,
		RunE:  groupHelp,
	}
	cmd.AddCommand(newVcsAuthStatusCmd(), newVcsAuthTokenCmd())
	return cmd
}

func newVcsAuthStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status [<owner/name>...]",
		Short: "Report the identity each repository's GitHub reads and writes resolve to, with its quota",
		Long: `Report the identity each repository's GitHub reads and writes resolve to,
and the GraphQL quota each identity has left.
Each window's callers clear when it resets; every finished window stays in
ledger-history.jsonl beside the ledger, as one JSON object per line.

Reads (ccx vcs status, vcs pr status, vcs reviews, and ship's CI watch) go
through a GitHub App installation token when ~/.config/ccx/github-app.toml
names an app installed on the repository, so they draw on the installation's
quota instead of the user's. The file names the app and a command printing its
private key:

  name = "poetic-vulcan"
  client_id = "Iv23li0tgNc0O5JnGoEH"
  private_key_command = ["aws", "ssm", "get-parameter", "--name", "/ci/vulcan/github-app-private-key",
    "--with-decryption", "--query", "Parameter.Value", "--output", "text"]

Tokens are minted read-only, cached per installation until five minutes before
they expire, and shared by every ccx process on the machine. Reads stay on the
gh user's token when the app is not installed on the repository, when minting
fails, or when GH_TOKEN or GITHUB_TOKEN is set. Writes always use the gh user.

Each identity's pools (core, graphql, search) come fresh from GitHub's
rate_limit, which costs no quota, with the callers ccx and ccx vcs gh charged
to each since it last reset, heaviest first. GitHub can count one pool in two
windows at once, each with its own reset: rate_limit reports one, and the
rate-limit headers on GraphQL answers mostly report the other, so a pool's
line names every open window, each reset to the second when there are two.
GitHub refuses a request counted in a spent window whatever the other has
left, so a pool refuses reads while any open window has under 1000 left
and marks its line "under the floor"; --json carries each open window as
{"identity", "resource", "limit", "remaining", "floor", "resetAt", "readAt",
"callers": [{"caller", "requests"}]}, a pool with two windows as two entries.

With no repository named, reports the current checkout's.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVcsAuthStatus(cmd, args, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	return cmd
}

func newVcsAuthTokenCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "token [<owner/name>]",
		Short: "Print the GitHub App installation token ccx vcs gh reads a repository with",
		Long: `Print the read-only GitHub App installation token ccx vcs gh hands a gh read
of the repository, alone on stdout, so a script can export it as GH_TOKEN:

  GH_TOKEN=$(ccx vcs auth token o/r)

The token comes from the machine-wide cache ccx's reads share, minted again
when it has under 50 minutes left. It is the app's token whatever GH_TOKEN or
GITHUB_TOKEN holds, and no rate limit floor applies, since minting draws on
no installation quota. When no app is configured, the app is not installed on
the repository, or minting fails, it prints nothing on stdout and exits 1
naming why. A failed mint keeps reads on the gh user for five minutes, and
this command fails for those five minutes too, naming the failure.

With --json it prints, on one line, the token with its app, account,
installation, expiry, and permissions: the file another machine's ccx reads
through when CCX_GITHUB_APP_TOKEN_FILE names it, in place of minting its own.
That token has at least 55 minutes left, so a pusher that renews the file every
few minutes keeps it over the 50 minutes a watch takes. It covers every
repository of the account the installation reaches.

With no repository named, reads the current checkout's.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runVcsAuthToken(cmd, args, asJSON) },
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the token with its expiry as the file CCX_GITHUB_APP_TOKEN_FILE names")
	return cmd
}

func runVcsAuthToken(cmd *cobra.Command, args []string, asJSON bool) error {
	ctx := cmd.Context()
	if len(args) == 0 {
		looked, err := vcs.LookupRepo(ctx, render.Dir(workingDir(ctx)), false)
		if err != nil {
			return fmt.Errorf("auth token: name the repository: %w", err)
		}
		args = []string{looked.NameWithOwner}
	}
	margin := ghapi.AppWatchMargin
	if asJSON {
		margin = ghapi.AppPushMargin
	}
	pushed, err := reviewsAPI().ForRepo(args[0]).InstallationToken(ctx, margin)
	if err != nil {
		return fmt.Errorf("auth token: %w", err)
	}
	if asJSON {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(pushed)
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), pushed.Token)
	return err
}

func runVcsAuthStatus(cmd *cobra.Command, repos []string, asJSON bool) error {
	ctx := cmd.Context()
	if len(repos) == 0 {
		looked, err := vcs.LookupRepo(ctx, render.Dir(workingDir(ctx)), false)
		if err != nil {
			return fmt.Errorf("auth status: name the repository: %w", err)
		}
		repos = []string{looked.NameWithOwner}
	}
	reports := make([]ghapi.Auth, 0, len(repos))
	for _, repo := range repos {
		auth, err := reviewsAPI().ForRepo(repo).Auth(ctx)
		if err != nil {
			return fmt.Errorf("auth status: %s: %w", repo, err)
		}
		reports = append(reports, auth)
	}
	out := cmd.OutOrStdout()
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(reports)
	}
	for _, auth := range reports {
		writeAuth(out, auth)
	}
	return nil
}

func writeAuth(w io.Writer, auth ghapi.Auth) {
	_, _ = fmt.Fprintln(w, auth.Repo)
	reads := "  reads   " + authIdentity(auth.Reads)
	if auth.Reads.ExpiresAt != nil {
		reads += ", token valid until " + auth.Reads.ExpiresAt.Local().Format(time.Kitchen)
	}
	if auth.Reason != "" {
		reads += " (" + auth.Reason + ")"
	}
	_, _ = fmt.Fprintln(w, reads)
	_, _ = fmt.Fprintln(w, "  writes  "+authIdentity(auth.Writes))
	identities := []ghapi.Identity{auth.Writes}
	if auth.Reads.Kind != auth.Writes.Kind {
		identities = []ghapi.Identity{auth.Reads, auth.Writes}
	}
	label := "  quota   "
	for _, id := range identities {
		_, _ = fmt.Fprintln(w, label+authQuota(id))
		label = "          "
	}
	label = "  pools   "
	for _, id := range identities {
		for pools := id.Pools; len(pools) > 0; {
			n := 1
			for n < len(pools) && pools[n].Resource == pools[0].Resource {
				n++
			}
			resets := time.Kitchen
			if n > 1 {
				resets = "3:04:05PM"
			}
			windows := make([]string, 0, n)
			for _, p := range pools[:n] {
				windows = append(windows, fmt.Sprintf("%d/%d left, resets %s", p.Remaining, p.Limit, p.ResetAt.Local().Format(resets)))
			}
			line := fmt.Sprintf("%s %s %s", pools[0].Identity, pools[0].Resource, strings.Join(windows, " and "))
			if slices.ContainsFunc(pools[:n], func(p ghapi.Pool) bool { return p.Remaining < p.Floor }) {
				line += ", under the floor"
			}
			if top := ghapi.TopCallers(pools[:n], 5); top != "" {
				line += "; top callers: " + top
			}
			_, _ = fmt.Fprintln(w, label+line)
			label = "          "
			pools = pools[n:]
		}
	}
}

func authIdentity(id ghapi.Identity) string {
	if id.Installation != 0 {
		return fmt.Sprintf("%s %s (installation %d)", id.Kind, id.Name, id.Installation)
	}
	return id.Kind + " " + id.Name
}

func authQuota(id ghapi.Identity) string {
	return fmt.Sprintf("%s %s: %d/%d GraphQL left, resets %s",
		id.Kind, id.Name, id.Quota.Remaining, id.Quota.Limit, id.Quota.ResetAt.Local().Format(time.Kitchen))
}
