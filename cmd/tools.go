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
	toolsListJSON          bool
	toolsListAllNamespaces bool
	toolsListNoHeaders     bool

	// toolsNamespaceFlag backs the persistent --namespace flag on the tools
	// group. When set it overrides the effective context's namespace for the
	// tools subcommands.
	toolsNamespaceFlag string
)

// toolsNamespace returns the namespace the tools subcommands should use:
// the --namespace flag when given, otherwise the effective context's namespace
// (the --context override, else the current context).
func toolsNamespace() (string, error) {
	if toolsNamespaceFlag != "" {
		return toolsNamespaceFlag, nil
	}
	return currentNamespace()
}

var toolsListCmd = &cobra.Command{
	Use:   "list",
	Short: "List tools",
	Long: `List tools reported by the server (GET <server>/tools).

By default tools are listed in a single namespace — the tools --namespace
flag, or the current context's namespace. With --all-namespaces, the set of
namespaces is discovered from the server (GET <server>/namespaces) and tools
are listed across all of them, with a separate request per namespace.

The combined tools are printed as a single human-readable table with a
NAMESPACE column. With --json each namespace's raw response is printed
unchanged, separated by a line containing "---".

With --no-headers the column header row is omitted, so the output can be fed to
other tools:

  rossoctl tools list --no-headers | awk '{print $1}' | xargs -n1 rossoctl tools delete

--no-headers also moves the "no tools found" notice to stderr, leaving stdout
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
		//   otherwise        -> a single namespace (tools --namespace, else
		//                        the current context)
		var namespaces []string
		if toolsListAllNamespaces {
			nsResp, err := client.ListNamespaces(cmd.Context(), true)
			if err != nil {
				return err
			}
			namespaces = nsResp.Namespaces
		} else {
			ns, err := toolsNamespace()
			if err != nil {
				return err
			}
			namespaces = []string{ns}
		}

		responses := make([]*apiclient.ToolListResponse, 0, len(namespaces))
		for _, ns := range namespaces {
			resp, err := client.ListTools(cmd.Context(), ns)
			if err != nil {
				return err
			}
			responses = append(responses, resp)
		}

		if toolsListJSON {
			return printToolsJSON(cmd, responses)
		}

		// Combine all namespaces' tools into one table.
		var tools []apiclient.ToolSummary
		for _, resp := range responses {
			tools = append(tools, resp.Items...)
		}
		printToolsTable(cmd, tools)
		return nil
	},
}

// printToolsJSON prints each namespace's response as indented JSON, separated
// by a line containing "---".
func printToolsJSON(cmd *cobra.Command, responses []*apiclient.ToolListResponse) error {
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

// printToolsTable prints tools as a table, with a header row unless
// --no-headers was given.
//
// The empty-result notice goes to stderr under --no-headers and to stdout
// otherwise. The flag exists so the output can be piped, and a pipeline reading
// "No tools found." gets the word "No" as its first argument; stdout has to be
// empty when there are no rows. Interactively there is no pipe to protect, and
// moving the line unconditionally would hide it from anyone redirecting stdout to
// a file, so the two cases differ. This mirrors kubectl, which likewise reports
// "No resources found" on stderr.
func printToolsTable(cmd *cobra.Command, tools []apiclient.ToolSummary) {
	out := cmd.OutOrStdout()

	if len(tools) == 0 {
		if toolsListNoHeaders {
			fmt.Fprintln(cmd.ErrOrStderr(), "No tools found.")
			return
		}
		fmt.Fprintln(out, "No tools found.")
		return
	}

	// Stable ordering: namespace then name.
	sort.Slice(tools, func(i, j int) bool {
		if tools[i].Namespace != tools[j].Namespace {
			return tools[i].Namespace < tools[j].Namespace
		}
		return tools[i].Name < tools[j].Name
	})

	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	if !toolsListNoHeaders {
		fmt.Fprintln(w, "NAME\tNAMESPACE\tSTATUS\tWORKLOAD\tPROTOCOL\tDESCRIPTION")
	}
	for _, tl := range tools {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			tl.Name,
			tl.Namespace,
			tl.Status,
			deref(tl.WorkloadType),
			strings.Join(tl.Labels.Protocol, ","),
			truncate(tl.Description),
		)
	}
	_ = w.Flush()
}

func init() {
	toolsCmd := newGroup("tools", "Manage tools")

	// Persistent so every tools subcommand inherits them. No -n shorthand for
	// --namespace, matching the agents group.
	toolsCmd.PersistentFlags().StringVar(&toolsNamespaceFlag, "namespace", "",
		"namespace for tools subcommands (overrides the context's namespace)")

	toolsListCmd.Flags().BoolVar(&toolsListJSON, "json", false, "print the raw JSON response unchanged")
	toolsListCmd.Flags().BoolVarP(&toolsListAllNamespaces, "all-namespaces", "A", false, "list tools across all namespaces discovered from the server")
	toolsListCmd.Flags().BoolVar(&toolsListNoHeaders, "no-headers", false, "omit the column header row, so the output can be piped to other tools")

	toolsCmd.AddCommand(
		toolsListCmd,
		toolsGetCmd,
		newToolsImportCmd(),
		toolsDeleteCmd,
		toolsWaitCmd,
	)
	rootCmd.AddCommand(toolsCmd)
}
