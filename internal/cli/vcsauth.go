package cli

import (
	"encoding/json"
	"fmt"
	"io"
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
	cmd.AddCommand(newVcsAuthStatusCmd())
	return cmd
}

func newVcsAuthStatusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status [<owner/name>...]",
		Short: "Report the identity each repository's GitHub reads and writes resolve to, with its quota",
		Long: `Report the identity each repository's GitHub reads and writes resolve to,
and the GraphQL quota each identity has left.

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

With no repository named, reports the current checkout's.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVcsAuthStatus(cmd, args, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the report as JSON")
	return cmd
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
	if auth.Reads.Kind != auth.Writes.Kind {
		_, _ = fmt.Fprintln(w, "  quota   "+authQuota(auth.Reads))
		_, _ = fmt.Fprintln(w, "          "+authQuota(auth.Writes))
		return
	}
	_, _ = fmt.Fprintln(w, "  quota   "+authQuota(auth.Writes))
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
