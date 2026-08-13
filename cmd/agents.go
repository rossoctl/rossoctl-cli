package cmd

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

var (
	agentsListJSON          bool
	agentsListAllNamespaces bool
	agentsListNoHeaders     bool

	// agentsNamespaceFlag backs the persistent --namespace flag on the agents
	// group. When set it overrides the effective context's namespace for the
	// agents subcommands.
	agentsNamespaceFlag string
)

// agentsNamespace returns the namespace the agents subcommands should use:
// the --namespace flag when given, otherwise the effective context's namespace
// (the --context override, else the current context).
func agentsNamespace() (string, error) {
	if agentsNamespaceFlag != "" {
		return agentsNamespaceFlag, nil
	}
	return currentNamespace()
}

var agentsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List agents",
	Long: `List agents reported by the server (GET <server>/agents).

By default agents are listed in a single namespace — the agents --namespace
flag, or the current context's namespace. With --all-namespaces, the set of
namespaces is discovered from the server (GET <server>/namespaces) and agents
are listed across all of them, with a separate request per namespace.

The combined agents are printed as a single human-readable table with a
NAMESPACE column. With --json each namespace's raw response is printed
unchanged, separated by a line containing "---".

With --no-headers the column header row is omitted, so the output can be fed to
other tools:

  rossoctl agents list --no-headers | awk '{print $1}' | xargs -n1 rossoctl agents delete

--no-headers also moves the "no agents found" notice to stderr, leaving stdout
empty when there is nothing to list, so a pipeline sees no rows rather than a
sentence. It has no effect with --json, which prints no headers to begin with.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		client, err := newClient(cmd)
		if err != nil {
			return err
		}

		// Namespace selection:
		//   --all-namespaces -> discover via GET /namespaces (list across all)
		//   otherwise        -> a single namespace (agents --namespace, else
		//                        the current context)
		var namespaces []string
		if agentsListAllNamespaces {
			nsResp, err := client.ListNamespaces(cmd.Context(), true)
			if err != nil {
				return err
			}
			namespaces = nsResp.Namespaces
		} else {
			ns, err := agentsNamespace()
			if err != nil {
				return err
			}
			namespaces = []string{ns}
		}

		responses := make([]*apiclient.AgentListResponse, 0, len(namespaces))
		for _, ns := range namespaces {
			resp, err := client.ListAgents(cmd.Context(), ns)
			if err != nil {
				return err
			}
			responses = append(responses, resp)
		}

		if agentsListJSON {
			return printAgentsJSON(cmd, responses)
		}

		// Combine all namespaces' agents into one table.
		var agents []apiclient.AgentSummary
		for _, resp := range responses {
			agents = append(agents, resp.Items...)
		}
		printAgentsTable(cmd, agents)
		return nil
	},
}

// printAgentsJSON prints each namespace's response as indented JSON, separated
// by a line containing "---".
func printAgentsJSON(cmd *cobra.Command, responses []*apiclient.AgentListResponse) error {
	out := cmd.OutOrStdout()
	for i, resp := range responses {
		if i > 0 {
			fmt.Fprintln(out, "---")
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return nil
}

// printAgentsTable prints agents as a table, with a header row unless
// --no-headers was given. See printToolsTable for why the empty-result notice
// changes stream with the flag.
func printAgentsTable(cmd *cobra.Command, agents []apiclient.AgentSummary) {
	out := cmd.OutOrStdout()

	if len(agents) == 0 {
		if agentsListNoHeaders {
			fmt.Fprintln(cmd.ErrOrStderr(), "No agents found.")
			return
		}
		fmt.Fprintln(out, "No agents found.")
		return
	}

	// Stable ordering: namespace then name.
	sort.Slice(agents, func(i, j int) bool {
		if agents[i].Namespace != agents[j].Namespace {
			return agents[i].Namespace < agents[j].Namespace
		}
		return agents[i].Name < agents[j].Name
	})

	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	if !agentsListNoHeaders {
		fmt.Fprintln(w, "NAME\tNAMESPACE\tSTATUS\tWORKLOAD\tPROTOCOL\tDESCRIPTION")
	}
	for _, a := range agents {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			a.Name,
			a.Namespace,
			a.Status,
			deref(a.WorkloadType),
			strings.Join(a.Labels.Protocol, ","),
			truncate(a.Description),
		)
	}
	_ = w.Flush()
}

// deref returns the pointed-to string, or "-" when the pointer is nil.
func deref(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

// truncate shortens s to at most 30 characters for table display: strings
// longer than 30 are cut to their first 27 characters with "..." appended.
func truncate(s string) string {
	const max = 30
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func init() {
	agentsCmd := newGroup("agents", "Manage agents")

	// Persistent so every agents subcommand inherits them. No -n shorthand for
	// --namespace: that belongs to `agents list --namespaces`.
	agentsCmd.PersistentFlags().StringVar(&agentsNamespaceFlag, "namespace", "",
		"namespace for agents subcommands (overrides the context's namespace)")

	agentsListCmd.Flags().BoolVar(&agentsListJSON, "json", false, "print the raw JSON response unchanged")
	agentsListCmd.Flags().BoolVarP(&agentsListAllNamespaces, "all-namespaces", "A", false, "list agents across all namespaces discovered from the server")
	agentsListCmd.Flags().BoolVar(&agentsListNoHeaders, "no-headers", false, "omit the column header row, so the output can be piped to other tools")

	agentsCardCmd.Flags().BoolVar(&agentsCardJSON, "json", false, "print the raw JSON response unchanged")

	chatFlags := agentsChatCmd.Flags()
	chatFlags.StringVar(&agentsChatArgs.address, "address", "",
		"URL of the A2A agent (default: the URL from the agent's card)")
	chatFlags.StringVar(&agentsChatArgs.transport, "transport", defaultA2ATransport,
		"protocol binding to use: jsonrpc, grpc, or http+json")
	chatFlags.StringVar(&agentsChatArgs.message, "message", "", "message text to send (required)")
	chatFlags.BoolVar(&agentsChatArgs.withAuthorization, "with-authorization", false,
		"attach the context's bearer token as an Authorization header")

	agentsCmd.AddCommand(
		newAgentsAuthbridgeCmd(),
		agentsCardCmd,
		agentsChatCmd,
		agentsDeleteCmd,
		newAgentsImportCmd(),
		agentsGetCmd,
		agentsListCmd,
		agentsWaitCmd,
	)
	rootCmd.AddCommand(agentsCmd)
}
