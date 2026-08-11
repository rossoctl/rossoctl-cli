package cmd

import (
	"encoding/json"
	"fmt"
	"slices"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/config"
	"github.com/rossoctl/rossoctl-cli/internal/instances"
	"github.com/rossoctl/rossoctl-cli/internal/rossoctlclient"
	"github.com/rossoctl/rossoctl-cli/internal/serve"
)

// loadConfig returns the context config, creating and seeding it from the
// effective default server if it does not yet exist. This is the lazy
// create-if-missing behavior: the file is only touched when a context command
// needs to inspect it.
func loadConfig() (*config.Config, error) {
	path, err := config.DefaultPath()
	if err != nil {
		return nil, err
	}
	return config.EnsureContext(path, serverOrDefault())
}

// loadConfigReadOnly loads the context config without creating or seeding it.
// A missing file yields an empty Config. It is used by commands that inspect
// or select existing contexts (get-contexts, use-context) and by the --context
// override, none of which should ever bring a context into existence.
func loadConfigReadOnly() (*config.Config, error) {
	path, err := config.DefaultPath()
	if err != nil {
		return nil, err
	}
	return config.Load(path)
}

// --- get-contexts ---

var configGetContextsJSON bool

var configGetContextsCmd = &cobra.Command{
	Use:   "get-contexts",
	Short: "List configured contexts",
	Long: `List the contexts persisted in ~/.config/rossoctl/config.yaml.

Listing never creates a context: if the config file does not exist or is empty,
an empty list is shown. With --json the raw config is printed unchanged.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfigReadOnly()
		if err != nil {
			return err
		}

		if configGetContextsJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(cfg)
		}

		out := cmd.OutOrStdout()
		w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
		fmt.Fprintln(w, "CURRENT\tNAME\tTYPE\tSERVER\tNAMESPACE\tTOKEN")
		for _, c := range cfg.Contexts {
			marker := ""
			if c.Name == cfg.CurrentContext {
				marker = "*"
			}
			typ := string(c.Type)
			if typ == "" {
				// Contexts written before the type field existed are treated as
				// api, the historical default.
				typ = string(config.TypeAPI)
			}
			namespace := c.Namespace
			if namespace == "" {
				namespace = "-"
			}
			token := "<none>"
			if c.BearerToken != "" {
				token = "<set>"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", marker, c.Name, typ, c.Server, namespace, token)
		}
		return w.Flush()
	},
}

// --- use-context ---

var configUseContextCmd = &cobra.Command{
	Use:   "use-context <name>",
	Short: "Set the current context",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		// Read-only: use-context selects an existing context and must never
		// create one. SetCurrent errors if name is unknown.
		cfg, err := loadConfigReadOnly()
		if err != nil {
			return err
		}
		if err := cfg.SetCurrent(name); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		cmd.Printf("Switched to context %q.\n", name)
		return nil
	},
}

// --- delete-context ---

var configDeleteContextCmd = &cobra.Command{
	Use:   "delete-context <name>",
	Short: "Delete a context",
	Long: `Delete the named context from ~/.config/rossoctl/config.yaml.

Deleting never creates the config: it errors if the named context does not
exist. If the deleted context was the current one, no context is current
afterward.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		// Read-only load: delete-context operates on an existing context and
		// must never bring the config into existence. Delete errors if name is
		// unknown.
		cfg, err := loadConfigReadOnly()
		if err != nil {
			return err
		}
		wasCurrent := cfg.CurrentContext == name
		if err := cfg.Delete(name); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		if wasCurrent {
			cmd.Printf("Deleted context %q (it was current; no context is current now).\n", name)
		} else {
			cmd.Printf("Deleted context %q.\n", name)
		}
		return nil
	},
}

// --- create-context ---

var (
	createContextName      string
	createContextServer    string
	createContextNamespace string
	createContextToken     string
)

var configCreateContextCmd = &cobra.Command{
	Use:   "create-context",
	Short: "Create a context and make it current",
	Long: `Create (or replace) a named context and make it the current context.

--server sets the context's server URI; if omitted, the global --server value
is used, falling back to the built-in default. --namespace and --bearer-token
are optional.

A context named "cortex" is answered by handlers inside the command's own
process rather than over the network, so creating one also creates the ` +
		defaultServeNamespaces + ` namespace directories under
~/.config/rossoctl/namespaces. That gives the new context somewhere to start
agents and tools, and makes those namespaces visible to "namespaces list"
before anything is running in them.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if createContextName == "" {
			return fmt.Errorf("--name is required")
		}

		serverURI := createContextServer
		if serverURI == "" {
			serverURI = serverOrDefault()
		}

		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		cfg.Upsert(config.Context{
			Name:        createContextName,
			Type:        config.TypeAPI,
			Server:      serverURI,
			Namespace:   createContextNamespace,
			BearerToken: createContextToken,
		})
		// Creating a context makes it the current one.
		if err := cfg.SetCurrent(createContextName); err != nil {
			return err
		}
		if err := cfg.Save(); err != nil {
			return err
		}
		cmd.Printf("Created context %q and set it as current.\n", createContextName)

		if createContextName == rossoctlclient.CortexContextName {
			if err := seedCortexNamespaces(cmd); err != nil {
				return err
			}
		}
		return nil
	},
}

// seedCortexNamespaces creates the default namespace directories for a newly
// created cortex context, and names them on stdout.
//
// It runs after the config is saved, so a failure here leaves a usable context
// rather than discarding one that was already reported as created. The namespaces
// are the same list `cortex serve` advertises by default, taken from that flag's
// default so the two cannot drift.
//
// A failure is reported rather than ignored: the directories are what make the
// namespaces selectable, and silently skipping them would leave a context whose
// namespace does not exist.
func seedCortexNamespaces(cmd *cobra.Command) error {
	for _, ns := range serve.SplitNamespaces(defaultServeNamespaces) {
		dir, err := instances.CreateNamespace(ns)
		if err != nil {
			return fmt.Errorf("creating namespace %q for the cortex context: %w", ns, err)
		}
		cmd.Printf("Created namespace %q (%s).\n", ns, dir)
	}
	return nil
}

// --- set-context ---

var (
	setContextNamespace string
	setContextName      string
)

var configSetContextCmd = &cobra.Command{
	Use:   "set-context",
	Short: "Update the current context",
	Long: `Update the current context.

--namespace sets its namespace (checked against GET <server>/namespaces; a
warning is printed if it is not among them, but it is set regardless).
--server replaces its server URI. --name renames the current context, updating
the current-context reference to the new name. At least one of these must be
given.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		changedNamespace := cmd.Flags().Changed("namespace")
		changedServer := cmd.Flags().Changed("server")
		changedName := cmd.Flags().Changed("name")
		if !changedNamespace && !changedServer && !changedName {
			return fmt.Errorf("nothing to do: give at least one of --namespace, --server, or --name")
		}

		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		cur, ok := cfg.Current()
		if !ok {
			return fmt.Errorf("no current context")
		}
		oldName := cur.Name

		if changedNamespace {
			// Warn (but don't fail) if the namespace is not one the server
			// knows. serverKnowsNamespace resolves the server the same way
			// other commands do, so an explicit --server is used for this too.
			if known, err := serverKnowsNamespace(cmd, setContextNamespace); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"Warning: could not verify namespace against the server: %v\n", err)
			} else if !known {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"Warning: namespace %q is not among the server's namespaces\n", setContextNamespace)
			}
			cur.Namespace = setContextNamespace
		}

		// Replace the context's server only when --server was given explicitly
		// (not left at its default).
		if changedServer {
			cur.Server = server
		}

		// Rename last so the earlier cur.* mutations land on the same context;
		// Rename updates the current-context reference when it applies.
		if changedName {
			if err := cfg.Rename(oldName, setContextName); err != nil {
				return err
			}
		}

		if err := cfg.Save(); err != nil {
			return err
		}
		cmd.Printf("Updated context %q.\n", finalContextName(oldName, changedName, setContextName))
		return nil
	},
}

// finalContextName returns the context's name after set-context: the new name
// when renamed, otherwise the original.
func finalContextName(oldName string, renamed bool, newName string) string {
	if renamed {
		return newName
	}
	return oldName
}

// serverKnowsNamespace reports whether ns is among the namespaces the server
// returns from GET <server>/namespaces (enabled namespaces only).
func serverKnowsNamespace(cmd *cobra.Command, ns string) (bool, error) {
	client, err := newClient(cmd)
	if err != nil {
		return false, err
	}
	resp, err := client.ListNamespaces(cmd.Context(), true)
	if err != nil {
		return false, err
	}
	return slices.Contains(resp.Namespaces, ns), nil
}

func init() {
	configCmd := newGroup("config", "Manage rossoctl contexts")

	configGetContextsCmd.Flags().BoolVar(&configGetContextsJSON, "json", false, "print the raw config as JSON")

	configCreateContextCmd.Flags().StringVar(&createContextName, "name", "", "name of the context (required)")
	configCreateContextCmd.Flags().StringVar(&createContextServer, "server", "", "server URI for the context (default: global --server or built-in default)")
	configCreateContextCmd.Flags().StringVar(&createContextNamespace, "namespace", "", "optional default namespace for the context")
	configCreateContextCmd.Flags().StringVar(&createContextToken, "bearer-token", "", "optional bearer token for the context")

	configSetContextCmd.Flags().StringVar(&setContextNamespace, "namespace", "", "namespace to set on the current context")
	configSetContextCmd.Flags().StringVar(&setContextName, "name", "", "rename the current context to this name")

	configCmd.AddCommand(
		configGetContextsCmd,
		configUseContextCmd,
		configDeleteContextCmd,
		configCreateContextCmd,
		configSetContextCmd,
	)
	rootCmd.AddCommand(configCmd)
}
