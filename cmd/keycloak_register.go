package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/keycloakadmin"
)

// Flags specific to `keycloak register`.
var (
	registerTrustDomain    string
	registerNamespace      string
	registerSA             string
	registerWorkload       string
	registerAuthType       string
	registerSpiffeIDPAlias string
	registerTokenExchange  bool
	registerPlatforms      []string
)

var keycloakRegisterCmd = &cobra.Command{
	Use:   "register",
	Short: "Create a workload's OIDC client and audience scope",
	Long: `Create the Keycloak objects a workload needs for OIDC authentication.

This does what the operator's client registration does, for a workload the
operator has not registered — four objects:

  - an OAuth client whose clientId is the workload's SPIFFE ID
  - a client scope "agent-<namespace>-<workload>-aud" holding an audience mapper
  - a realm-level default-default-client-scope entry for that scope
  - a default-client-scope attachment on this client and each platform client

The audience the mapper writes is the client's own SPIFFE ID, so an access token
issued to the workload names the workload as its audience — which is what the
workload's inbound JWT validation checks.

An existing client is reused, never modified: its clientId is keyed on the
ServiceAccount rather than the workload, so it may be shared by sibling
workloads. If it differs from what this command would have created, the
difference is reported rather than corrected.

Safe to re-run: existing objects are reused and the attachments are idempotent,
so a partially completed registration is finished by running this again.

The namespace defaults to the current context's namespace. Credentials are the
Keycloak admin account, not the rossoctl login.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if registerWorkload == "" {
			return fmt.Errorf("--workload is required")
		}
		if registerTrustDomain == "" {
			return fmt.Errorf("--trustDomain must not be empty")
		}
		if err := keycloakadmin.ValidateRealm(keycloakRealm); err != nil {
			return fmt.Errorf("--realm: %w", err)
		}

		namespace := registerNamespace
		if namespace == "" {
			var err error
			namespace, err = currentNamespace()
			if err != nil {
				return err
			}
		}

		// The same three names the operator derives, and that `keycloak
		// unregister` deletes.
		clientID := keycloakadmin.SpiffeClientID(registerTrustDomain, namespace, registerSA)
		clientName := namespace + "/" + registerWorkload
		scopeName := keycloakadmin.AudienceScopeName(namespace, registerWorkload)

		kc := &keycloakadmin.Client{BaseURL: keycloakURL}
		if verbose {
			kc.Logf = func(format string, args ...any) {
				fmt.Fprintf(cmd.ErrOrStderr(), format+"\n", args...)
			}
		}

		token, err := kc.PasswordGrantToken(cmd.Context(), adminUser, adminPass)
		if err != nil {
			return err
		}

		res, err := kc.Register(cmd.Context(), token, keycloakadmin.RegisterOptions{
			Realm:      keycloakRealm,
			ClientID:   clientID,
			ClientName: clientName,
			ScopeName:  scopeName,
			// The operator uses the client's own ID as the audience.
			Audience:            clientID,
			AuthType:            registerAuthType,
			SpiffeIDPAlias:      registerSpiffeIDPAlias,
			TokenExchangeEnable: registerTokenExchange,
			Platforms:           registerPlatforms,
		})
		if err != nil {
			return err
		}

		printRegisterResult(cmd, res, scopeName)
		return nil
	},
}

// printRegisterResult reports what was created, distinguishing that from what was
// already there.
//
// The distinction matters: a user running this to fix a broken registration needs
// to know whether the object they suspect was rebuilt or merely found.
func printRegisterResult(cmd *cobra.Command, res keycloakadmin.RegisterResult, scopeName string) {
	switch {
	case res.ClientCreated:
		cmd.Printf("Created client %q.\n", res.ClientID)
	case res.ClientExisted:
		cmd.Printf("Client %q already exists; reused.\n", res.ClientID)
	}

	// Reported for both cases: in federated-jwt mode the absence of a secret is
	// itself the thing a user needs to know, since they may be looking for one.
	switch res.AuthType {
	case keycloakadmin.AuthTypeFederatedJWT:
		cmd.Println("  auth: federated-jwt (SPIFFE JWT-SVID; no client secret)")
	case keycloakadmin.AuthTypeClientSecret:
		if res.ClientSecret != "" {
			cmd.Printf("  client secret: %s\n", res.ClientSecret)
		} else {
			cmd.Println("  auth: client-secret (Keycloak returned no secret)")
		}
	}

	// Printed after the client so it is not lost above the secret, and worded as a
	// warning because a mismatched client authenticates nothing.
	for _, d := range res.Drift {
		cmd.Printf("  warning: %s\n", d)
	}

	switch {
	case res.ScopeCreated:
		cmd.Printf("Created audience scope %q.\n", scopeName)
	case res.ScopeExisted:
		cmd.Printf("Audience scope %q already exists; reused.\n", scopeName)
		// Said explicitly: the mapper carries the audience, and this command does
		// not touch an existing scope's mappers, so a wrong audience persists.
		cmd.Println("  its existing audience mapper was left unchanged")
	}

	if res.RealmLinked {
		cmd.Println("Linked it as a realm default client scope.")
	}
	if res.ClientLinked {
		cmd.Println("Attached it to the client.")
	}
	for _, plat := range res.PlatformLinked {
		cmd.Printf("Attached it to platform client %q.\n", plat)
	}

	// A token minted before this registration does not carry the new audience
	// scope, so an existing login keeps failing the workload's JWT validation
	// until it is replaced. Say so, since the registration itself looked like it
	// succeeded and the next failure would otherwise be mystifying.
	cmd.Println("\nRun `rossoctl login` again so your token picks up the new audience scope.")
}

func init() {
	f := keycloakRegisterCmd.Flags()
	f.StringVar(&registerTrustDomain, "trustDomain", defaultTrustDomain,
		"SPIFFE trust domain of the workload's identity")
	f.StringVar(&registerNamespace, "namespace", "",
		"namespace of the workload (default: the current context's namespace)")
	f.StringVar(&registerSA, "sa", "default",
		"ServiceAccount the workload runs as")
	f.StringVar(&registerWorkload, "workload", "",
		"name of the workload to register (required)")
	f.StringVar(&registerAuthType, "clientAuthType", keycloakadmin.AuthTypeFederatedJWT,
		`how the client authenticates: "federated-jwt" (SPIFFE JWT-SVID) or "client-secret"`)
	f.StringVar(&registerSpiffeIDPAlias, "spiffeIDPAlias", keycloakadmin.DefaultSpiffeIDPAlias,
		"Keycloak identity provider alias trusted to issue the client's JWT (federated-jwt only)")
	f.BoolVar(&registerTokenExchange, "tokenExchange", false,
		"enable standard token exchange, letting the workload trade its token for a downstream one")
	f.StringSliceVar(&registerPlatforms, "platformClientID", keycloakadmin.DefaultPlatformClientIDs,
		"platform clientIds to attach the audience scope to")
}
