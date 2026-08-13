package cmd

import (
	"context"
	"time"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

var agentsWaitTimeout time.Duration

var agentsWaitCmd = &cobra.Command{
	Use:   "wait <name>",
	Short: "Wait for an agent to become ready",
	Long: `Wait until an agent reports a ready status
(polling GET <server>/agents/<namespace>/<name> every 2 seconds), where namespace
is the agents --namespace flag or the current context's namespace.

Exits 0 as soon as the agent is ready, so it can gate the automation that
follows:

    rossoctl agents import from-image --name orders --containerImage IMAGE \
      && rossoctl agents wait orders --timeout 5m \
      && ./run-integration-tests.sh

Nothing is printed while waiting unless --verbose is set. --timeout bounds the
wait and defaults to 60s; --timeout 0 waits indefinitely.

Waiting ends early, non-zero, when the agent reports a status readiness will
never follow: a failed job or a rollout that exceeded its deadline. Reporting
that immediately is more useful than spending the timeout to report the wrong
cause. A name the server does not know (404) also fails immediately rather than
waiting for a resource that does not exist, so a wait issued before its agent
exists fails instead of racing it.`,
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

		fetch := func(ctx context.Context) (*apiclient.AgentDetail, error) {
			return client.GetAgent(ctx, namespace, name)
		}
		if err := waitForReady(cmd, fetch, "agent", namespace, name, agentsWaitTimeout); err != nil {
			return err
		}

		cmd.Printf("Agent %q in namespace %q is ready.\n", name, namespace)
		return nil
	},
}

func init() {
	agentsWaitCmd.Flags().DurationVar(&agentsWaitTimeout, "timeout", defaultWaitTimeout,
		"how long to wait for the agent to become ready (0 waits indefinitely)")
}
