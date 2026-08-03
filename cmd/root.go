// Package cmd defines the command tree for the rossoctl CLI.
//
// Each command lives in its own file in this package and is attached to
// rootCmd in that file's init function. Commands stay thin: they parse and
// validate flags, then delegate to packages under internal/. This separation
// keeps the CLI surface (flags, help text, wiring) apart from the logic it
// drives, so the logic can be unit-tested without invoking Cobra.
package cmd

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
	"github.com/rossoctl/rossoctl-cli/internal/config"
	"github.com/rossoctl/rossoctl-cli/internal/cortexclient"
	"github.com/rossoctl/rossoctl-cli/internal/rossoctlclient"
)

// These are set at build time via -ldflags. See the Makefile.
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// defaultServer is the API endpoint used when --server is not supplied.
const defaultServer = "http://rossoctl-ui.localtest.me:8080/api/v1/"

// Persistent flags shared by every command.
var (
	verbose bool
	server  string
)

// rootCmd is the base command invoked when the binary is run with no
// subcommand. It carries no RunE of its own; running it prints help.
var rootCmd = &cobra.Command{
	Use:   "rossoctl",
	Short: "rossoctl controls Rosso resources",
	Long: `rossoctl is a command-line interface for interacting with Rosso.

It follows the standard Go CLI layout: this cmd package wires up the Cobra
command tree, while the actual work is implemented in packages under internal/.`,
	// SilenceUsage/SilenceErrors let us render errors ourselves in Execute
	// rather than printing the full usage text on every runtime error.
	SilenceUsage:  true,
	SilenceErrors: true,
}

// serverOrDefault returns the explicitly supplied --server value, or the
// built-in default when --server was not given. Used as the seed value when a
// context must be created.
func serverOrDefault() string {
	if server != "" {
		return server
	}
	return defaultServer
}

// contextOverride is the name of a context to use instead of the current one.
// It is bound to the root command's persistent --context flag, so every command
// inherits it; empty means "use the current context".
var contextOverride string

// resolveContext returns the effective context: the one named by --context
// when that flag is set, otherwise the current context.
//
// --context selects an existing context and must never create one, so its
// lookup uses a read-only load and errors if the named context does not exist.
// The current-context path, by contrast, seeds a default context on first use
// so resource commands work out of the box.
func resolveContext() (*config.Context, error) {
	if contextOverride != "" {
		cfg, err := loadConfigReadOnly()
		if err != nil {
			return nil, err
		}
		ctx, ok := cfg.Get(contextOverride)
		if !ok {
			return nil, fmt.Errorf("no context named %q", contextOverride)
		}
		return ctx, nil
	}
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	cur, ok := cfg.Current()
	if !ok {
		// EnsureContext (via loadConfig) guarantees a current context; treat
		// its absence as a programming error rather than silently falling back.
		return nil, fmt.Errorf("no current context")
	}
	return cur, nil
}

// resolveServer determines the effective server URI and bearer token for a
// command:
//
//   - An explicit (non-empty) --server wins and overrides any context; no
//     token is used in that case.
//   - Otherwise the effective context (see resolveContext: --context, else the
//     current context) supplies both the server URI and its bearer token.
func resolveServer() (serverURI, token string, err error) {
	if server != "" {
		return server, "", nil
	}
	ctx, err := resolveContext()
	if err != nil {
		return "", "", err
	}
	return ctx.Server, ctx.BearerToken, nil
}

// currentNamespace returns the namespace of the effective context (see
// resolveContext). It returns an error if that context has no namespace set,
// since callers that need a namespace cannot proceed without one.
func currentNamespace() (string, error) {
	ctx, err := resolveContext()
	if err != nil {
		return "", err
	}
	if ctx.Namespace == "" {
		return "", fmt.Errorf("context %q has no namespace set; run `rossoctl login` to sign in and select one", ctx.Name)
	}
	return ctx.Namespace, nil
}

// newClient builds a Rossoctl backend for the effective context, delegating
// construction (and the api-vs-cortex dispatch) to rossoctlclient.NewClient.
//
// An explicit --server overrides any context and always targets the HTTP API,
// so it is modeled as a transient api context with that server and no token.
// Otherwise the effective context (see resolveContext) is used as-is.
//
// When --verbose is set, a logger writing one line per operation to the
// command's stderr is attached to the backend, so verbose output never mixes
// with the --json/table results on stdout.
func newClient(cmd *cobra.Command) (rossoctlclient.Rossoctl, error) {
	var ctx *config.Context
	if server != "" {
		ctx = &config.Context{Type: config.TypeAPI, Server: server}
	} else {
		c, err := resolveContext()
		if err != nil {
			return nil, err
		}
		ctx = c
	}

	client := rossoctlclient.NewClient(ctx)
	attachVerboseLogger(cmd, client)
	return client, nil
}

// attachVerboseLogger wires a stderr logger onto client when --verbose is set.
// It handles both backends: the HTTP apiclient.Client and the file-backed
// cortexclient.FileClient each expose a Logf hook.
func attachVerboseLogger(cmd *cobra.Command, client rossoctlclient.Rossoctl) {
	if !verbose {
		return
	}
	errOut := cmd.ErrOrStderr()
	logf := func(format string, args ...any) {
		fmt.Fprintf(errOut, format+"\n", args...)
	}
	switch c := client.(type) {
	case *apiclient.Client:
		c.Logf = logf
	case *cortexclient.FileClient:
		c.Logf = logf
	}
}

// Execute runs the root command. It is the single entry point called from
// main and is the only exported symbol in this package.
//
// Most failures print an "Error:" line and exit 1. A command that ran a child
// process to completion instead reports the child's status via *exitCodeError:
// that status becomes ours, and nothing is printed, since the child has already
// said whatever it had to say on its own stderr.
func Execute() {
	err := rootCmd.Execute()
	if err == nil {
		return
	}

	var exitErr *exitCodeError
	if errors.As(err, &exitErr) {
		os.Exit(exitErr.code)
	}

	fmt.Fprintln(os.Stderr, "Error:", err)
	os.Exit(1)
}

func init() {
	rootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "enable verbose output")
	rootCmd.PersistentFlags().StringVar(&server, "server", "", "Rossoctl API server URI (overrides the current context; default: current context's server)")
	rootCmd.PersistentFlags().StringVar(&contextOverride, "context", "",
		"use this context instead of the current one")
}
