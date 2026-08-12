package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
	"github.com/rossoctl/rossoctl-cli/internal/rossoctlclient"
)

var (
	agentsAuthbridgeSetPolicyFile string
	agentsAuthbridgeSetWait       bool
)

// authbridgeWaitInterval is the poll period while waiting for AuthBridge to
// report a change, and authbridgeWaitTimeout bounds the wait.
//
// The wait is capped rather than unbounded because the common reason it never
// converges is a pod that is not coming back, and a `set --wait` in CI would
// otherwise hang the job instead of failing it. Both are variables rather than
// constants so tests can shorten them.
var (
	authbridgeWaitInterval = 2 * time.Second
	authbridgeWaitTimeout  = 2 * time.Minute
)

var agentsAuthbridgeSetCmd = &cobra.Command{
	Use:   "set <name>",
	Short: "Set an agent's AuthBridge configuration from a policy file",
	Long: `Store an AuthBridge configuration for an agent
(PUT <server>/agents/<namespace>/<name>/identity-config), where namespace is the
agents --namespace flag or the current context's namespace.

--policy-file is required and names a local file. Its bytes are sent verbatim as
text/plain: the server writes them into the agent's authbridge-config-<name>
ConfigMap, so the file is not parsed, reformatted, or validated here. Comments
and key order survive, and anything malformed is reported by the server rather
than guessed at locally.

Writing the ConfigMap is not the same as the running AuthBridge picking it up.
With --wait the command reads the current configuration before the PUT, then
polls (GET <server>/agents/<namespace>/<name>/identity-config) every 2 seconds
until what AuthBridge reports differs from that baseline, giving up after 2
minutes.

The poll compares each response against the pre-PUT baseline rather than against
the file, because the two are not comparable: the file is YAML written to a
ConfigMap, while the GET returns the live JSON a sidecar serves, with secrets
redacted. A change relative to the baseline is the signal that the new
configuration was loaded.

Because the signal is a change, --wait cannot confirm a PUT that changes nothing.
Re-applying the configuration that is already live times out and exits non-zero,
reporting that no change was seen.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		namespace, err := agentsNamespace()
		if err != nil {
			return err
		}

		// Read the file before any request: a typo in the path should not leave a
		// half-done operation behind, and it costs nothing to find out first.
		policy, err := os.ReadFile(agentsAuthbridgeSetPolicyFile)
		if err != nil {
			return fmt.Errorf("reading --policy-file: %w", err)
		}

		client, err := newClient(cmd)
		if err != nil {
			return err
		}

		out := cmd.OutOrStdout()

		// The baseline is only read under --wait, where it is the thing the poll
		// compares against. Without the flag it has no purpose, and fetching it
		// anyway would make `set` fail for an agent whose sidecar is unreachable
		// even though the PUT itself would have succeeded.
		var baseline *apiclient.AgentIdentityConfig
		if agentsAuthbridgeSetWait {
			baseline, err = client.GetAgentIdentityConfig(cmd.Context(), namespace, name)
			if err != nil {
				// Abort without writing. A failure here means the agent name is
				// wrong or the cluster is unreachable, and in both cases a PUT
				// would either fail too or write a configuration nobody asked
				// for at that path.
				return fmt.Errorf("reading the current configuration before writing: %w", err)
			}
		}

		if _, err := client.PutAgentIdentityConfig(cmd.Context(), namespace, name, policy); err != nil {
			return err
		}

		if !agentsAuthbridgeSetWait {
			fmt.Fprintf(out, "Wrote %s to agent %q in namespace %q.\n",
				agentsAuthbridgeSetPolicyFile, name, namespace)
			return nil
		}

		fmt.Fprintf(out, "Wrote %s to agent %q in namespace %q; waiting for AuthBridge to report the change...\n",
			agentsAuthbridgeSetPolicyFile, name, namespace)

		if err := waitForIdentityConfigChange(cmd, client, namespace, name, baseline); err != nil {
			return err
		}

		fmt.Fprintln(out, "AuthBridge reported the new configuration.")
		return nil
	},
}

// errWaitTimeout is returned when the poll ran out of time. It names the
// no-op case explicitly, since re-applying the live configuration is the most
// likely innocent reason for it and the one a user is most likely to hit.
var errWaitTimeout = errors.New("the configuration was written, but AuthBridge reported no change within the timeout;\n" +
	"the policy may already have been in effect, or the agent's pods may not be running.\n" +
	"Check with `rossoctl agents authbridge get <name>`")

// waitForIdentityConfigChange polls the identity-config endpoint until it
// reports something other than baseline, or the timeout expires.
//
// A failed GET during the poll is not fatal: the endpoint reads from the agent's
// own pods, which are precisely what a configuration change restarts, so a
// transient error or a 404 mid-rollout is expected. Only the timeout ends the
// wait unsuccessfully.
func waitForIdentityConfigChange(
	cmd *cobra.Command,
	client rossoctlclient.Rossoctl,
	namespace, name string,
	baseline *apiclient.AgentIdentityConfig,
) error {
	ctx := cmd.Context()
	deadline := timeNow().Add(authbridgeWaitTimeout)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeAfter(authbridgeWaitInterval):
		}

		current, err := client.GetAgentIdentityConfig(ctx, namespace, name)
		if err == nil && identityConfigChanged(baseline, current) {
			return nil
		}

		if timeNow().After(deadline) {
			return errWaitTimeout
		}
	}
}

// identityConfigChanged reports whether the configuration AuthBridge serves has
// changed relative to the pre-PUT baseline.
//
// Comparison is semantic rather than byte-wise, over the JSON both responses
// decode to. Re-encoding a decoded response would put keys in a fixed order and
// so hide nothing, but comparing raw bytes would report a change for a response
// the server merely serialized differently.
//
// The injected "AuthBridge" key is excluded: the server sets it to true whenever
// a sidecar answered and false when none did, so it tracks pod reachability
// rather than configuration. Leaving it in would make a pod restarting during a
// rollout look like the configuration change we are waiting for.
func identityConfigChanged(baseline, current *apiclient.AgentIdentityConfig) bool {
	return !reflect.DeepEqual(comparableConfig(baseline), comparableConfig(current))
}

// comparableConfig renders a configuration as a generic JSON value with the
// injected AuthBridge key removed, so two of them can be compared without regard
// to key order or numeric formatting.
//
// A configuration that cannot be re-encoded compares as nil. That is safe here:
// the only consequence is that this poll round reports no change, and the next
// one tries again.
func comparableConfig(cfg *apiclient.AgentIdentityConfig) any {
	if cfg == nil {
		return nil
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil
	}
	var generic any
	if err := json.Unmarshal(data, &generic); err != nil {
		return nil
	}
	if m, ok := generic.(map[string]any); ok {
		delete(m, "AuthBridge")
	}
	return generic
}

// timeNow and timeAfter are indirected so the wait can be tested without
// sleeping in real time.
var (
	timeNow   = time.Now
	timeAfter = time.After
)
