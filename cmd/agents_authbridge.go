package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

var agentsAuthbridgeJSON bool

// newAgentsAuthbridgeCmd builds the `agents authbridge` group. It holds only
// `get` today; the group exists so that reading an agent's AuthBridge
// configuration and the operations that will act on it are siblings under one
// noun, rather than the read being the whole of `agents authbridge` and every
// later addition having to displace it.
func newAgentsAuthbridgeCmd() *cobra.Command {
	authbridgeCmd := newGroup("authbridge", "Inspect an agent's AuthBridge configuration")

	agentsAuthbridgeGetCmd.Flags().BoolVar(&agentsAuthbridgeJSON, "json", false,
		"print the raw JSON response unchanged")

	authbridgeCmd.AddCommand(agentsAuthbridgeGetCmd)
	return authbridgeCmd
}

var agentsAuthbridgeGetCmd = &cobra.Command{
	Use:   "get <name>",
	Short: "Show an agent's AuthBridge identity configuration",
	Long: `Show the AuthBridge configuration for an agent
(GET <server>/agents/<namespace>/<name>/identity-config), where namespace is the
agents --namespace flag or the current context's namespace.

This is the configuration AuthBridge itself runs from: the mode, and the inbound
and outbound plugin pipelines with each plugin's per-instance configuration.
Plugins are listed in execution order, which is the order the pipeline invokes
them and so the order in which one plugin sees another's effects.

Each plugin's on_error policy is reported alongside it, because it changes what
the plugin does rather than merely how a failure is logged: "off" means the
plugin is not dispatched at all, and "observe" runs it but discards its verdict.
A plugin listed without either is enforcing.

By default the configuration is printed as text. With --json the raw JSON
returned by the server is printed unchanged.`,
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
		cfg, err := client.GetAgentIdentityConfig(cmd.Context(), namespace, name)
		if err != nil {
			return err
		}

		if agentsAuthbridgeJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(cfg)
		}

		printIdentityConfig(cmd.OutOrStdout(), cfg)
		return nil
	},
}

// printIdentityConfig renders an agent's AuthBridge configuration as text: the
// mode, then the inbound and outbound plugin pipelines.
//
// Both stages are always printed, including when empty. An empty pipeline is a
// real and consequential state — authlib's own startup warnings flag it, since a
// stage with no plugins means the listener has nothing to invoke — so it is
// reported as "(none)" rather than by omitting the section, which would look the
// same as a stage this command failed to render.
func printIdentityConfig(out io.Writer, cfg *apiclient.AgentIdentityConfig) {
	// A mode is required by authlib's validation, but this value comes from the
	// server, so an absent one is reported as unset rather than printed blank.
	fmt.Fprintf(out, "Mode: %s\n", orDefault(cfg.Mode, "(unset)"))

	printPipelineStage(out, "Inbound Plugins", cfg.Pipeline.Inbound)
	printPipelineStage(out, "Outbound Plugins", cfg.Pipeline.Outbound)
}

// printPipelineStage renders one stage's plugins in execution order.
//
// Each plugin is numbered, since order is part of the meaning here: the pipeline
// invokes them in sequence, so "2." is not decoration but the position in which
// this plugin runs relative to the others.
func printPipelineStage(out io.Writer, heading string, stage apiclient.PipelineStage) {
	section(out, heading)

	if len(stage.Plugins) == 0 {
		fmt.Fprintln(out, "  (none)")
		return
	}

	for i, p := range stage.Plugins {
		if i > 0 {
			fmt.Fprintln(out)
		}

		// The name is what identifies a plugin; an entry without one is
		// malformed rather than anonymous, so it is labelled as such instead of
		// printing a bare number with nothing beside it.
		line := fmt.Sprintf("  %d. %s", i+1, orDefault(p.Name, "(unnamed)"))
		// An explicit id distinguishes two entries of the same plugin, so it is
		// only shown when it adds something the name does not.
		if p.ID != "" {
			line += fmt.Sprintf(" (id: %s)", p.ID)
		}
		// Empty means enforce (authlib resolves it that way), so the default is
		// named explicitly rather than left blank: "no policy shown" and
		// "enforcing" would otherwise be indistinguishable in the output.
		policy := string(p.OnError)
		line += fmt.Sprintf(" [on_error: %s]", orDefault(policy, "enforce"))
		fmt.Fprintln(out, line)

		printPluginConfig(out, p.Config)
	}
}

// printPluginConfig renders a plugin's per-instance configuration block.
//
// The config is a json.RawMessage that the plugin framework leaves uninterpreted
// — each plugin owns its own schema — so this re-indents the JSON rather than
// decoding it into fields. That keeps the output faithful to configuration for
// plugins this build of the CLI knows nothing about, which is most of them.
func printPluginConfig(out io.Writer, raw json.RawMessage) {
	// A plugin with no config block is the common case (most plugins take none),
	// and authlib normalizes an explicit `config: null` to nil, so both arrive
	// here as an absent block rather than as a literal "null" to print.
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		fmt.Fprintln(out, "     config: (none)")
		return
	}

	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "     ", "  "); err != nil {
		// The bytes came from the server and are not necessarily valid JSON.
		// Print them as-is: unparseable config is exactly the kind of thing
		// worth seeing, and hiding it behind an error would make this command
		// useless for diagnosing it.
		fmt.Fprintf(out, "     config (unparseable): %s\n", bytes.TrimSpace(raw))
		return
	}

	fmt.Fprintf(out, "     config: %s\n", buf.String())
}
