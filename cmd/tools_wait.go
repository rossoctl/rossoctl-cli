package cmd

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

var toolsWaitTimeout time.Duration

var toolsWaitCmd = &cobra.Command{
	Use:   "wait <name>",
	Short: "Wait for a tool to become ready",
	Long: `Wait until a tool reports a ready status
(polling GET <server>/tools/<namespace>/<name> every 2 seconds), where namespace
is the tools --namespace flag or the current context's namespace.

Exits 0 as soon as the tool is ready, so it can gate the automation that follows:

    rossoctl tools import from-source --name weather --repoUrl URL \
      && rossoctl tools wait weather --timeout 10m \
      && rossoctl agents wait orders

Nothing is printed while waiting unless --verbose is set. --timeout bounds the
wait and defaults to 60s; --timeout 0 waits indefinitely.

A tool imported from source is built before it is deployed, and reports
"Building" for as long as that takes — often longer than the default timeout, so
allow for it with --timeout. If the build fails the tool reports "Build Failed",
and waiting ends early and non-zero rather than spending the remaining timeout on
a workload that will never be created. The same applies to a failed deployment,
and to a name the server does not know (404).

Against the local "cortex" context this runs to its timeout: that server does not
implement the tool detail endpoint this polls, and answers 500 rather than a
status. Use "rossoctl tools list" there instead. "rossoctl agents wait" does work
against cortex.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		namespace, err := toolsNamespace()
		if err != nil {
			return err
		}

		client, err := newClient(cmd)
		if err != nil {
			return err
		}

		fetch := func(ctx context.Context) (*apiclient.AgentDetail, error) {
			return client.GetTool(ctx, namespace, name)
		}
		if err := waitForReady(cmd, fetch, "tool", namespace, name, toolsWaitTimeout); err != nil {
			return err
		}

		cmd.Printf("Tool %q in namespace %q is ready.\n", name, namespace)
		return nil
	},
}

func init() {
	toolsWaitCmd.Flags().DurationVar(&toolsWaitTimeout, "timeout", defaultWaitTimeout,
		"how long to wait for the tool to become ready (0 waits indefinitely)")
}
