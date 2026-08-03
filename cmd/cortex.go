package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/config"
	"github.com/rossoctl/rossoctl-cli/internal/cortexclient"
)

// cortexName is the name of the cortex to operate on, bound to the cortex
// group's --cortex flag. It doubles as the name of the cortex-typed context:
// when no matching context exists, one is created with this name.
var cortexName string

// defaultCortexName is the cortex (and context) name used when --cortex is not
// given.
const defaultCortexName = "default"

// Defaults for the `cortex start` flags, mirroring rossoctlx's _add_start_args.
const (
	defaultProxyPort   = 8185
	defaultControlPort = 8186
	defaultBudget      = 5.0
	defaultCortexImage = "quay.io/aslomnet/rosscortex:latest"
)

// startArgs holds the values of the `cortex start` flags. These mirror
// rossoctlx's _add_start_args; they are registered on the command but not yet
// acted upon (start currently only resolves/creates the context).
var startArgs struct {
	port         int
	controlPort  int
	upstream     string
	budget       float64
	local        bool
	noAuthbridge bool
	image        string
	logFollow    bool
}

// addStartArgs registers the start flags on cmd, mirroring rossoctlx's
// _add_start_args (same names, defaults, and help text).
func addStartArgs(cmd *cobra.Command) {
	f := cmd.Flags()
	f.IntVar(&startArgs.port, "port", defaultProxyPort, "Proxy listen port")
	f.IntVar(&startArgs.controlPort, "control-port", defaultControlPort, "Control API port")
	f.StringVar(&startArgs.upstream, "upstream", "", "Upstream LiteLLM URL")
	f.Float64Var(&startArgs.budget, "budget", defaultBudget, "Global daily budget in USD")
	f.BoolVar(&startArgs.local, "local", false, "Run locally (uses ROSSOCORTEX_CONTAINER_LOCAL_DIR or rossocortex-container/)")
	f.BoolVar(&startArgs.noAuthbridge, "no-authbridge", false, "Direct mode without AuthBridge (local only)")
	f.StringVar(&startArgs.image, "image", defaultCortexImage, "Container image (default mode)")
	f.BoolVarP(&startArgs.logFollow, "log-follow", "f", false, "After starting, follow the log (like 'start' then 'log -f')")
}

// resolveCortexContext returns the cortex-typed context these commands operate
// on, creating it when necessary.
//
//   - With an explicit --context, that named context must already exist and be
//     of type cortex; it is never created and does not become current.
//   - Otherwise the context named by --cortex (default "default") is used: if a
//     context of that name exists it must be of type cortex; if none exists a
//     cortex-typed context is created with that name and made current.
//
// When a context is created and seedNamespace is true, its namespace is set to
// the first namespace the cortex backend reports (see firstCortexNamespace).
// This is best-effort: if the backend reports none, the namespace is left
// blank.
//
// The config is saved when a context is created or made current.
func resolveCortexContext(cmd *cobra.Command, seedNamespace bool) (*config.Context, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}

	// --context selects an existing context and must never create one.
	if contextOverride != "" {
		ctx, ok := cfg.Get(contextOverride)
		if !ok {
			return nil, fmt.Errorf("no context named %q", contextOverride)
		}
		if ctx.Type != config.TypeCortex {
			return nil, fmt.Errorf("context %q is of type %q, not %q", ctx.Name, ctx.Type, config.TypeCortex)
		}
		return ctx, nil
	}

	// Otherwise use (or create) the cortex context named by --cortex.
	if ctx, ok := cfg.Get(cortexName); ok {
		if ctx.Type != config.TypeCortex {
			return nil, fmt.Errorf("context %q is of type %q, not %q", ctx.Name, ctx.Type, config.TypeCortex)
		}
		// Ensure it is current, saving only if that changed anything.
		if cfg.CurrentContext != ctx.Name {
			if err := cfg.SetCurrent(ctx.Name); err != nil {
				return nil, err
			}
			if err := cfg.Save(); err != nil {
				return nil, err
			}
		}
		return ctx, nil
	}

	// No such context: create a cortex-typed one and make it current. On
	// creation, seed its namespace from the cortex backend when asked to.
	newCtx := config.Context{Name: cortexName, Type: config.TypeCortex}
	if seedNamespace {
		newCtx.Namespace = firstCortexNamespace(cmd, &newCtx)
	}
	cfg.Upsert(newCtx)
	if err := cfg.SetCurrent(cortexName); err != nil {
		return nil, err
	}
	if err := cfg.Save(); err != nil {
		return nil, err
	}
	ctx, _ := cfg.Get(cortexName)
	return ctx, nil
}

// firstCortexNamespace returns the first namespace the cortex backend reports
// for target, or "" if it cannot be determined (list error or none available).
// It builds a file-backed cortex client for the target, mirroring how login
// seeds a namespace from a server.
func firstCortexNamespace(cmd *cobra.Command, target *config.Context) string {
	client := cortexclient.NewFileClient(target.Name)
	attachVerboseLogger(cmd, client)
	resp, err := client.ListNamespaces(cmd.Context(), true)
	if err != nil || len(resp.Namespaces) == 0 {
		return ""
	}
	return resp.Namespaces[0]
}

var cortexStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start a cortex",
	Long: `Start the cortex named by --cortex (default "default").

The cortex is addressed through a context of type "cortex". If --context names
an existing cortex context, that context is used. Otherwise the context named
by --cortex is used, creating a cortex-typed context of that name and making it
current when none exists.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// start seeds the namespace of a newly-created cortex context from the
		// backend's first reported namespace.
		ctx, err := resolveCortexContext(cmd, true)
		if err != nil {
			return err
		}
		cmd.Printf("Started cortex %q (context %q).\n", ctx.Name, ctx.Name)
		return nil
	},
}

var cortexStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop a cortex",
	Long: `Stop the cortex named by --cortex (default "default").

The cortex is addressed through a context of type "cortex". If --context names
an existing cortex context, that context is used. Otherwise the context named
by --cortex is used, creating a cortex-typed context of that name and making it
current when none exists.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, err := resolveCortexContext(cmd, false)
		if err != nil {
			return err
		}
		cmd.Printf("Stopped cortex %q (context %q).\n", ctx.Name, ctx.Name)
		return nil
	},
}

var cortexStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the status of a cortex",
	Long: `Show the status of the cortex named by --cortex (default "default").

The cortex is addressed through a context of type "cortex". If --context names
an existing cortex context, that context is used. Otherwise the context named
by --cortex is used, creating a cortex-typed context of that name and making it
current when none exists.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, err := resolveCortexContext(cmd, false)
		if err != nil {
			return err
		}
		cmd.Printf("Cortex %q (context %q).\n", ctx.Name, ctx.Name)
		return nil
	},
}

func init() {
	cortexCmd := newGroup("cortex", "Manage cortexes")

	// Hidden: the cortex commands are not part of the advertised CLI surface
	// today. They still work when named explicitly — hiding only removes them
	// from help listings and completion.
	cortexCmd.Hidden = true

	// Persistent so every cortex subcommand inherits it. --context is a
	// root-level persistent flag inherited here; for cortex it selects an
	// existing cortex-typed context instead of --cortex.
	cortexCmd.PersistentFlags().StringVar(&cortexName, "cortex", defaultCortexName,
		"name of the cortex to operate on")

	// start-specific flags (mirroring rossoctlx's _add_start_args).
	addStartArgs(cortexStartCmd)

	cortexCmd.AddCommand(
		cortexStartCmd,
		cortexStopCmd,
		cortexStatusCmd,
		cortexDoctorCmd,
	)
	rootCmd.AddCommand(cortexCmd)
}
