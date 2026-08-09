package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/keycloakadmin"
)

// Flags specific to `keycloak unregister`.
var (
	unregisterTrustDomain string
	unregisterNamespace   string
	unregisterSA          string
	unregisterWorkload    string
	unregisterForce       bool
	unregisterPlatforms   []string
)

// defaultTrustDomain is the SPIFFE trust domain of the local development setup,
// matching defaultKeycloakURL's host.
const defaultTrustDomain = "localtest.me"

var keycloakUnregisterCmd = &cobra.Command{
	Use:   "unregister",
	Short: "Remove a workload's OIDC client and audience scope",
	Long: `Remove the Keycloak objects created when a workload was registered for OIDC.

This is the inverse of the operator's client registration, which creates more
than an OAuth client:

  - a client scope "agent-<namespace>-<workload>-aud" holding an audience mapper
  - a realm-level default-default-client-scope entry for that scope
  - a default-client-scope attachment on each platform client

The last two are realm-global and live on long-lived shared clients, so nothing
removes them when a workload is deleted — the realm entry makes every client
created afterwards inherit the retired workload's audience scope. Those are the
objects this command exists to clean up.

The audience scope is per-workload and is always removed. The OAuth client is
NOT removed by default: its clientId is the workload's SPIFFE ID, which the
operator keys on the ServiceAccount rather than the workload, so a namespace's
workloads sharing a ServiceAccount share one client and deleting it would break
the siblings. Pass --force to delete it as well.

The namespace defaults to the current context's namespace. Credentials are the
Keycloak admin account, not the rossoctl login.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if unregisterWorkload == "" {
			return fmt.Errorf("--workload is required")
		}
		if unregisterTrustDomain == "" {
			return fmt.Errorf("--trustDomain must not be empty")
		}
		if err := keycloakadmin.ValidateRealm(keycloakRealm); err != nil {
			return fmt.Errorf("--realm: %w", err)
		}

		namespace := unregisterNamespace
		if namespace == "" {
			var err error
			namespace, err = currentNamespace()
			if err != nil {
				return err
			}
		}

		// The two names the operator derives, reproduced rather than guessed at.
		clientID := keycloakadmin.SpiffeClientID(unregisterTrustDomain, namespace, unregisterSA)
		scopeName := keycloakadmin.AudienceScopeName(namespace, unregisterWorkload)

		kc := &keycloakadmin.Client{BaseURL: keycloakURL}
		if verbose {
			// Verbose output goes to stderr so it never mixes with the summary.
			kc.Logf = func(format string, args ...any) {
				fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...)
			}
		}

		token, err := kc.PasswordGrantToken(cmd.Context(), adminUser, adminPass)
		if err != nil {
			return err
		}

		res, err := kc.Unregister(cmd.Context(), token, keycloakadmin.UnregisterOptions{
			Realm:        keycloakRealm,
			ClientID:     clientID,
			ScopeName:    scopeName,
			Platforms:    unregisterPlatforms,
			DeleteClient: unregisterForce,
		})
		if err != nil {
			return err
		}

		printUnregisterResult(cmd, res, scopeName)
		return nil
	},
}

// printUnregisterResult reports what was removed.
//
// Every step is reported, including the ones that found nothing: a user running
// this to clean up after a failure needs to know which objects were already
// gone, and silence would read as success for work that never happened.
func printUnregisterResult(cmd *cobra.Command, res keycloakadmin.Result, scopeName string) {
	if res.ScopeDeleted {
		cmd.Printf("Deleted client scope %q.\n", scopeName)
		if res.RealmLinkRemoved {
			cmd.Println("Removed its realm default-default-client-scope link.")
		}
		for _, plat := range res.PlatformUnlinked {
			cmd.Printf("Removed its scope link on platform client %q.\n", plat)
		}
	}
	if res.ScopeAbsent {
		cmd.Printf("Client scope %q not found; nothing to remove.\n", scopeName)
	}

	switch {
	case res.ClientDeleted:
		cmd.Printf("Deleted client %q.\n", res.ClientID)
	case res.ClientAbsent:
		cmd.Printf("Client %q not found.\n", res.ClientID)
	case res.ClientSkipped:
		// Not an error, but the whole point of the default, so it is stated rather
		// than left for the user to infer from the absence of a line.
		cmd.Printf("Client %q was NOT deleted: it is keyed on the ServiceAccount,\n", res.ClientID)
		cmd.Println("so other workloads using it would lose authentication.")
		cmd.Println("Re-run with --force to delete it.")
	}
}

func init() {
	f := keycloakUnregisterCmd.Flags()
	f.StringVar(&unregisterTrustDomain, "trustDomain", defaultTrustDomain,
		"SPIFFE trust domain of the workload's identity")
	f.StringVar(&unregisterNamespace, "namespace", "",
		"namespace of the workload (default: the current context's namespace)")
	f.StringVar(&unregisterSA, "sa", "default",
		"ServiceAccount the workload runs as")
	f.StringVar(&unregisterWorkload, "workload", "",
		"name of the workload to unregister (required)")
	f.BoolVar(&unregisterForce, "force", false,
		"also delete the OAuth client, which may be shared by workloads using the same ServiceAccount")
	f.StringSliceVar(&unregisterPlatforms, "platformClientID", keycloakadmin.DefaultPlatformClientIDs,
		"platform clientIds to unlink the audience scope from")
}
