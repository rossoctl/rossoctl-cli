package cmd

// Flags shared by every keycloak subcommand, bound to the keycloak group as
// persistent flags.
var (
	keycloakURL   string
	keycloakRealm string
	adminUser     string
	adminPass     string
)

const (
	// defaultKeycloakURL is the Keycloak in the local development setup.
	defaultKeycloakURL = "http://keycloak.localtest.me:8080"

	// defaultKeycloakRealm is the realm the operator registers workloads in.
	//
	// The operator reads this per namespace from the authbridge-config ConfigMap
	// key KEYCLOAK_REALM, which this CLI has no access to, so it cannot be
	// discovered here. "rossoctl" is the value the deployment uses; --realm covers
	// an installation that chose another.
	defaultKeycloakRealm = "rossoctl"

	// defaultAdminUser and defaultAdminPass are the development Keycloak's
	// built-in admin credentials.
	defaultAdminUser = "admin"
	defaultAdminPass = "admin"
)

func init() {
	keycloakCmd := newGroup("keycloak", "Manage OIDC registrations in Keycloak")
	keycloakCmd.Long = `Manage the Keycloak objects that back workload OIDC authentication.

These commands talk to the Keycloak admin REST API directly, not to the Rosso
API server, and authenticate with the master realm's admin account rather than
the credentials from ` + "`rossoctl login`" + `.`

	// Persistent so every keycloak subcommand inherits them.
	keycloakCmd.PersistentFlags().StringVar(&keycloakURL, "keycloakURL", defaultKeycloakURL,
		"base URL of the Keycloak server")
	keycloakCmd.PersistentFlags().StringVar(&keycloakRealm, "realm", defaultKeycloakRealm,
		"Keycloak realm the workload is registered in")
	keycloakCmd.PersistentFlags().StringVar(&adminUser, "adminUser", defaultAdminUser,
		"Keycloak admin username (master realm)")
	keycloakCmd.PersistentFlags().StringVar(&adminPass, "adminPass", defaultAdminPass,
		"Keycloak admin password (master realm)")

	keycloakCmd.AddCommand(keycloakRegisterCmd)
	keycloakCmd.AddCommand(keycloakUnregisterCmd)
	rootCmd.AddCommand(keycloakCmd)
}
