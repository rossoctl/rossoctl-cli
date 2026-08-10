package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/config"
	"github.com/rossoctl/rossoctl-cli/internal/jwt"
)

var authStatusJSON bool

// authStatusCmd inspects the token rossoctl already holds, rather than asking
// the server about it. `rossoctl status` covers the server's view (GET
// /auth/status, /auth/me); this covers what the local credential itself
// asserts — which is what a user needs when the question is "why is the server
// refusing me" and the answer is in the token: it expired, it names the wrong
// audience, or it carries none of the roles the operation wants.
var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the claims in the current context's bearer token",
	Long: `Decode the bearer token stored on the effective context and show its claims.

Nothing is sent to the server: the token is read from
~/.config/rossoctl/config.yaml and decoded locally, so this works even against a
server that is down or that is rejecting the token. Use ` + "`rossoctl status`" + ` for
the server's view of the session.

Reported, when the token carries them: the user's name, preferred username, and
email; the issuer; the expiration time (with a warning when it has passed); the
audiences; the realm_access roles; and the scopes.

The token's signature is NOT verified — these claims are what the token says
about itself, not a statement that the server accepts it. A token that is
expired here will be rejected by the server; one that looks fine here may still
be rejected for reasons a signature check would catch.

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

		claims, err := jwt.Decode(ctx.BearerToken)
		if err != nil {
			// The stored token need not be a JWT — `login --token` accepts any
			// string, and an OAuth server may issue an opaque one. Name the
			// context so it is clear which credential could not be read, and
			// point at the command that does ask the server.
			return fmt.Errorf("cannot read the token on context %q: %v\n"+
				"Only JWT bearer tokens can be inspected locally; run `rossoctl status` to ask the server instead",
				ctx.Name, err)
		}

		if authStatusJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(claims)
		}

		printAuthStatus(cmd.OutOrStdout(), ctx, claims, time.Now())
		return nil
	},
}

// printAuthStatus renders the claims as single-column text, in the order the
// sections are documented: identity, issuer, expiry, audiences, roles, scopes.
//
// now is a parameter rather than read from the clock so the expiry warning is
// testable.
func printAuthStatus(out io.Writer, ctx *config.Context, c *jwt.Claims, now time.Time) {
	// The warning leads: a user running this because something is failing should
	// not have to read to the Expires line to find the reason.
	if c.Expired(now) {
		exp, _ := c.Expiry()
		fmt.Fprintf(out, "WARNING: this token expired %s (%s ago). Run `rossoctl login` to sign in again.\n",
			exp.Format(time.RFC3339), roundDuration(now.Sub(exp)))
	}

	section(out, "Token on context "+ctx.Name)
	rows := newRows()

	// Identity. Each line is omitted when its claim is absent rather than shown
	// as a placeholder: an absent claim is normal (a mapper that was not
	// configured), and a blank value would read as a broken account.
	addIfSet(rows, "Name", c.Name)
	addIfSet(rows, "Username", c.Username)
	addIfSet(rows, "Email", c.Email)
	addIfSet(rows, "Subject", c.Subject)

	addIfSet(rows, "Issuer", c.Issuer)

	if exp, ok := c.Expiry(); ok {
		rows.add("Expires", expiryLabel(exp, now))
	} else {
		// Said explicitly rather than omitted: a token with no expiry is
		// unusual enough that its absence is itself worth reporting.
		rows.add("Expires", "never (no exp claim)")
	}

	addList(rows, "Audiences", c.Audience)
	addList(rows, "Roles", c.Roles())
	addList(rows, "Scopes", c.Scopes())

	rows.flush(out)
}

// expiryLabel renders an expiry as a timestamp plus how far away it is, so the
// absolute time is available for correlating with logs and the relative time is
// readable at a glance.
func expiryLabel(exp, now time.Time) string {
	stamp := exp.Format(time.RFC3339)
	if !exp.After(now) {
		return fmt.Sprintf("%s (EXPIRED %s ago)", stamp, roundDuration(now.Sub(exp)))
	}
	return fmt.Sprintf("%s (in %s)", stamp, roundDuration(exp.Sub(now)))
}

// roundDuration renders d at second granularity, which is as precise as a
// NumericDate claim is anyway.
func roundDuration(d time.Duration) string {
	return d.Round(time.Second).String()
}

// addIfSet adds a row only when the value is non-blank. See printAuthStatus for
// why absent claims get no line.
func addIfSet(r *rows, term, value string) {
	if strings.TrimSpace(value) != "" {
		r.add(term, value)
	}
}

// addList adds a comma-joined row for a claim that holds a list, marking an
// empty one "(none)" rather than omitting it.
//
// Unlike the identity claims, emptiness here is the answer to the user's
// question: no audiences and no roles is exactly the state that explains a 403,
// so the line has to appear.
func addList(r *rows, term string, values []string) {
	if len(values) == 0 {
		r.add(term, "(none)")
		return
	}
	r.add(term, strings.Join(values, ", "))
}

func init() {
	authCmd := newGroup("auth", "Inspect rossoctl's stored credentials")
	authCmd.Long = `Inspect the credentials rossoctl has stored locally.

These commands read ~/.config/rossoctl/config.yaml and do not contact the
server. For the server's view of the session, use ` + "`rossoctl status`" + `; for the
server's authentication settings, use ` + "`rossoctl auth-config`" + `.`

	authStatusCmd.Flags().BoolVar(&authStatusJSON, "json", false,
		"print the decoded claims as JSON instead of human-formatted text")

	authCmd.AddCommand(authStatusCmd)
	rootCmd.AddCommand(authCmd)
}
