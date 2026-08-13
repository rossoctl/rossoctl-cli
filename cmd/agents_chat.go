package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

// agentsChatArgs holds the `agents chat` flags.
var agentsChatArgs struct {
	address           string
	transport         string
	message           string
	withAuthorization bool
}

var agentsChatCmd = &cobra.Command{
	Use:   "chat <name>",
	Short: "Send a message to an agent and stream the response",
	Long: `Send a message to a named agent and print the events it streams back.

This is ` + "`a2a send`" + ` addressed by agent name instead of by URL. Without
--address the agent's own URL is used: the one ` + "`agents card <name>`" + ` reports,
read from GET <server>/chat/<namespace>/<name>/agent-card, where namespace is the
agents --namespace flag or the current context's namespace. With --address that
lookup is skipped and the given URL is used directly, which is the way to reach
an agent whose card is unavailable or whose advertised URL is not reachable from
here.

Because the card is served by the agent itself and proxied by the backend, the
default path only works while the agent is running — an agent that is not ready
has no card, and so no URL to derive.

Note the message goes straight to the agent, not through the platform API, so the
card's hostname has to resolve from where this runs. A cluster-internal name
often does not, which fails with "no such host" even though the API server is
perfectly reachable; --address http://<route>:<port> is the way past it.

--transport, --message and --with-authorization mean what they do for
` + "`a2a send`" + `: the message text is sent as a single user text part, the
response is streamed event by event as it arrives, and --with-authorization
attaches the effective context's bearer token as an Authorization header on each
request. Note that the card lookup always carries the context's token, since it
goes to the platform API, while the message carries one only with
--with-authorization.

With --verbose both the card lookup and the message are reported on stderr.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]

		if agentsChatArgs.message == "" {
			return fmt.Errorf("--message is required")
		}

		address := agentsChatArgs.address
		if address == "" {
			var err error
			address, err = agentCardURL(cmd, name)
			if err != nil {
				return err
			}
		}

		// The namespace is resolved again here rather than threaded out of
		// agentCardURL because --address skips that call entirely, and the hint is
		// most useful in exactly that case's neighbour: an address, from wherever,
		// that does not resolve. A namespace that cannot be resolved yields no
		// example rather than failing the send.
		namespace, _ := agentsNamespace()

		return streamA2AMessage(cmd, a2aSendOptions{
			address:               address,
			transport:             agentsChatArgs.transport,
			message:               agentsChatArgs.message,
			withAuthorization:     agentsChatArgs.withAuthorization,
			unresolvedAddressHint: unresolvedAgentAddressHint(namespace, name),
		})
	},
}

// unresolvedAgentAddressHint returns the advice to offer when `agents chat`
// cannot resolve the address it is talking to.
//
// The failure is confusing because of what chat does not do: it does not relay
// the message through the platform API, it speaks A2A straight to the agent, at
// the URL the agent's own card advertises. So a working `agents get` and a
// reachable backend are no guarantee the address in the card resolves from here
// — a cluster-internal hostname routinely does not.
//
// The example is built from the agent's own namespace and name so it can be
// edited into a working flag rather than translated first. localtest.me is the
// local trust domain (see defaultTrustDomain) and resolves to 127.0.0.1, which
// is what a local gateway on :8080 fronts; a real cluster's route differs, hence
// "for example".
func unresolvedAgentAddressHint(namespace, name string) string {
	hint := "Hint: `agents chat` talks A2A directly to the agent, at the URL from its " +
		"agent-card endpoint — that hostname has to resolve from here, which a " +
		"cluster-internal one may not. Pass --address http://<route>:<port> to name a " +
		"reachable URL."
	if namespace == "" || name == "" {
		return hint
	}
	return fmt.Sprintf("%s\nFor example: --address http://%s.%s.localtest.me:8080",
		hint, name, namespace)
}

// agentCardURL returns the URL an agent advertises in its agent card — the same
// value `agents card <name>` prints as its URL row.
//
// The card is fetched through the same client and namespace resolution as
// `agents card`, so the two commands agree on which agent they mean and a
// failure to reach the backend is reported identically.
//
// An empty URL is an error rather than being passed on. The field is required by
// the response model but arrives from the agent, so a card can carry no URL at
// all; handing "" to the A2A client would fail later with a message about a
// malformed endpoint rather than about the card, and --address is the way past
// it.
func agentCardURL(cmd *cobra.Command, name string) (string, error) {
	namespace, err := agentsNamespace()
	if err != nil {
		return "", err
	}

	client, err := newClient(cmd)
	if err != nil {
		return "", err
	}
	card, err := client.GetAgentCard(cmd.Context(), namespace, name)
	if err != nil {
		return "", err
	}
	if card.URL == "" {
		return "", fmt.Errorf("agent %q reports no URL in its agent card; pass --address to name one", name)
	}
	return card.URL, nil
}
