package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

var agentsCardJSON bool

var agentsCardCmd = &cobra.Command{
	Use:   "card <name>",
	Short: "Show an agent's A2A agent card",
	Long: `Show an agent's A2A agent card
(GET <server>/chat/<namespace>/<name>/agent-card), where namespace is the
namespace of the current context.

The card is served by the agent itself and proxied by the backend, so it is only
available while the agent is running. An agent that is not ready has no card to
report.

By default the card is printed as text, laid out in the same sections as the web
UI's Agent Card panel. With --json the raw JSON returned by the server is printed
unchanged.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		namespace, err := agentsNamespace()
		if err != nil {
			return err
		}

		client, err := newClient(cmd)
		if err != nil {
			return err
		}
		card, err := client.GetAgentCard(cmd.Context(), namespace, name)
		if err != nil {
			return err
		}

		if agentsCardJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(card)
		}

		printAgentCard(cmd.OutOrStdout(), card)
		return nil
	},
}

// printAgentCard renders an agent card as text, mirroring the sections of the
// web UI's Agent Card panel: Basic Information, Description and Skills.
//
// Fields the UI renders conditionally are omitted here when empty rather than
// shown blank, for the same reason: the card comes from the agent, and a field
// it did not supply is absent rather than empty. The exception is Streaming,
// which the UI always shows because false is a meaningful answer for a boolean.
func printAgentCard(out io.Writer, c *apiclient.AgentCard) {
	fmt.Fprintf(out, "%s\n", c.Name)

	section(out, "Basic Information")
	r := newRows()
	r.add("Name", c.Name)
	r.add("Version", orDefault(c.Version, "N/A"))
	r.add("URL", orDefault(c.URL, "N/A"))
	// Always reported: unlike the strings above, "not streaming" is an answer
	// rather than a missing value, so the row stays even for a card that omitted
	// the field. enabledLabel is shared with `status`, which renders the same
	// Enabled/Disabled pair the UI uses.
	r.add("Streaming", enabledLabel(c.Streaming))
	r.flush(out)

	if c.Description != "" {
		section(out, "Description")
		// Printed as-is. The UI renders it as markdown; there is no terminal
		// equivalent, and reformatting it here would corrupt code blocks and
		// lists that are already readable as plain text.
		fmt.Fprintf(out, "%s\n", indentLines(c.Description, "  "))
	}

	if len(c.Skills) > 0 {
		section(out, "Skills")
		for i, s := range c.Skills {
			if i > 0 {
				fmt.Fprintln(out)
			}
			heading := orDefault(s.Name, orDefault(s.ID, "(unnamed)"))
			if len(s.Tags) > 0 {
				heading += fmt.Sprintf(" [%s]", strings.Join(s.Tags, ", "))
			}
			fmt.Fprintf(out, "  %s\n", heading)
			if s.Description != "" {
				fmt.Fprintf(out, "%s\n", indentLines(s.Description, "    "))
			}
			if len(s.Examples) > 0 {
				fmt.Fprintln(out, "    Examples:")
				for _, ex := range s.Examples {
					fmt.Fprintf(out, "      %s\n", ex)
				}
			}
		}
	}
}

// indentLines prefixes every line of s with indent, so a multi-line description
// stays within its section rather than starting at the left margin.
//
// Trailing whitespace is trimmed first so a description ending in a newline does
// not produce a line consisting only of the indent.
func indentLines(s, indent string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = indent + l
	}
	return strings.Join(lines, "\n")
}
