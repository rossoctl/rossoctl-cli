package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

// fetchEnvVars GETs envURL and parses its body as newline-separated key=value
// pairs into a slice of EnvVars. Blank lines and lines beginning with '#' are
// ignored. An empty envURL returns nil (no env vars).
//
// The current context's bearer token is sent only when envURL is on the same
// host as the API server, so a public env document (e.g. on GitHub) is fetched
// anonymously — sending the API token to a foreign host both leaks it and, for
// hosts like raw.githubusercontent.com, causes an unrelated 404.
func fetchEnvVars(ctx context.Context, cmd *cobra.Command, envURL string) ([]apiclient.EnvVar, error) {
	if envURL == "" {
		return nil, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, envURL, nil)
	if err != nil {
		return nil, err
	}
	if server, token, terr := resolveServer(); terr == nil && token != "" && sameHost(server, envURL) {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	if verbose {
		fmt.Fprintf(cmd.ErrOrStderr(), "GET %s (env vars)\n", envURL)
	}

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching env vars from %s: %w", envURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetching env vars from %s: HTTP %d", envURL, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("reading env vars from %s: %w", envURL, err)
	}

	return parseEnvVars(string(body))
}

// sameHost reports whether two URLs have the same host (including port). Used
// to decide whether the API bearer token may be sent to the env-vars URL.
func sameHost(a, b string) bool {
	ua, err := url.Parse(a)
	if err != nil {
		return false
	}
	ub, err := url.Parse(b)
	if err != nil {
		return false
	}
	return ua.Host != "" && ua.Host == ub.Host
}

// errNotKeyValue marks text that is not a key=value pair.
//
// Deliberately context-free: callers wrap it with where the text came from — a
// line number for a document, the flag value itself for --envVar — because a
// line number quoted at a single flag names text the user never typed.
var errNotKeyValue = errors.New("expected key=value")

// parseEnvVarPair interprets a single "key=value" string.
//
// This is the one place that defines what key=value means. Both the
// --envVarsURL document (via parseEnvVars) and each --envVar flag (via
// parseEnvVarFlags) go through it, so a line lifted out of an env document onto
// the command line yields the same EnvVar.
//
// Surrounding whitespace is trimmed from key and value; interior whitespace is
// preserved. Only the first '=' splits, so a value may contain more. An empty
// value is allowed — VAR= is a meaningful assignment — but a missing '=' or an
// empty key is not.
//
// Note what is *not* here: blank-line and '#'-comment skipping live in
// parseEnvVars, because those are conventions of an annotated document rather
// than of a pair. Moving them here would silently discard --envVar '#FOO=bar'.
func parseEnvVarPair(s string) (apiclient.EnvVar, error) {
	key, value, ok := strings.Cut(s, "=")
	key = strings.TrimSpace(key)
	if !ok || key == "" {
		return apiclient.EnvVar{}, errNotKeyValue
	}
	return apiclient.EnvVar{Name: key, Value: strings.TrimSpace(value)}, nil
}

// parseEnvVars parses newline-separated key=value pairs. Surrounding
// whitespace on each line is trimmed; blank lines and '#' comments are
// skipped. A line without '=' or with an empty key is an error.
func parseEnvVars(body string) ([]apiclient.EnvVar, error) {
	var out []apiclient.EnvVar
	for i, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ev, err := parseEnvVarPair(line)
		if err != nil {
			return nil, fmt.Errorf("invalid env var on line %d: %q (%w)", i+1, raw, err)
		}
		out = append(out, ev)
	}
	return out, nil
}

// parseEnvVarFlags converts repeated --envVar values into EnvVars. Each value is
// one key=value pair, interpreted exactly as a line of an --envVarsURL document
// is (see parseEnvVarPair) — except that nothing is skipped: '#' does not start
// a comment in a flag value, and an empty value is a value rather than a blank
// line.
//
// Errors quote the flag value the user actually typed, with no line number.
//
// Unlike parseServicePorts, an empty value is an error rather than skipped.
// That helper consumes a comma-split StringSlice, where a trailing comma
// manufactures empty entries; a StringArray entry exists only because the user
// typed the flag, so an empty --envVar is a mistake worth reporting.
func parseEnvVarFlags(values []string) ([]apiclient.EnvVar, error) {
	var out []apiclient.EnvVar
	for _, v := range values {
		ev, err := parseEnvVarPair(v)
		if err != nil {
			return nil, fmt.Errorf("invalid --envVar %q: %w", v, err)
		}
		out = append(out, ev)
	}
	return out, nil
}

// mergeEnvVars concatenates groups of env vars and resolves a repeated name
// last-wins, keeping each name in the slot where it first appeared: given
// FOO=a, BAR=b, FOO=c the result is [{FOO c} {BAR b}]. Holding the position
// keeps a document's own ordering stable when a later flag overrides one of its
// keys, so the difference between two request bodies is the one value that
// changed.
//
// Callers pass document pairs before flag pairs, so --envVar wins over
// --envVarsURL for the same name. That order is fixed rather than following the
// command line because pflag visits flags in lexical name order, which makes the
// real interleaving of the two flags unrecoverable.
//
// Variadic rather than two-argument so that a repeat *within* one group is
// resolved too — both --envVar FOO=1 --envVar FOO=2 and a document with a
// repeated key need last-wins.
//
// Returns nil, not an empty slice, when every group is empty: EnvVars is tagged
// omitempty, so an import with no env vars must send no envVars field at all.
func mergeEnvVars(groups ...[]apiclient.EnvVar) []apiclient.EnvVar {
	var out []apiclient.EnvVar
	at := make(map[string]int) // name -> its index in out
	for _, g := range groups {
		for _, ev := range g {
			if i, seen := at[ev.Name]; seen {
				out[i].Value = ev.Value
				continue
			}
			at[ev.Name] = len(out)
			out = append(out, ev)
		}
	}
	return out
}
