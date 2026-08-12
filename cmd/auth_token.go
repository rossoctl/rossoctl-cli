package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// authTokenCmd prints the stored bearer token and nothing else, so it can be
// substituted into another command:
//
//	curl -H "Authorization: Bearer $(rossoctl auth token)" ...
//
// This is the only command that writes a credential to stdout — `config
// get-contexts` deliberately renders one as <set>/<none>. That is the whole
// point of this command rather than an oversight, so the token is printed
// unconditionally, with no redaction and no TTY check: a value that is sometimes
// withheld cannot be relied on by a script, and `gh auth token` sets the
// precedent.
//
// Nothing is decoded. `auth status` covers "what does this token say"; this
// covers "give me the token", which must keep working for the opaque tokens
// `login --token` accepts and an OAuth server may issue.
var authTokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Print the effective context's bearer token",
	Long: `Print the bearer token stored on the effective context, and nothing else.

The token is read from ~/.config/rossoctl/config.yaml; nothing is sent to the
server. The output is the raw token followed by a newline, which command
substitution strips, so it can be passed straight to another tool:

  curl -H "Authorization: Bearer $(rossoctl auth token)" \
      http://my-host:8080/api/v1/agents?namespace=team1

Beware that this writes a credential to stdout, where a terminal will keep it in
scrollback and CI will keep it in the build log.

The token is not decoded or validated — an expired or opaque token is printed as
stored. Use ` + "`rossoctl auth status`" + ` to inspect a JWT's claims instead.

Exits non-zero when the context holds no token, so ` + "`$(rossoctl auth token)`" + `
fails loudly rather than expanding to an empty string.

The effective context is the one named by --context, else the current one.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, err := resolveContext()
		if err != nil {
			return err
		}
		if ctx.BearerToken == "" {
			return fmt.Errorf("context %q has no bearer token; run `rossoctl login` to sign in", ctx.Name)
		}

		fmt.Fprintln(cmd.OutOrStdout(), ctx.BearerToken)
		return nil
	},
}
