package cmd

import (
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
	"github.com/rossoctl/rossoctl-cli/internal/config"
	"github.com/rossoctl/rossoctl-cli/internal/deviceflow"
	"github.com/rossoctl/rossoctl-cli/internal/rossoctlclient"
)

var loginToken string

var loginCmd = &cobra.Command{
	Use:   "login",
	Short: "Log in and store a bearer token on a context",
	Long: `Obtain a bearer token and store it on a context in ~/.config/rossoctl/config.yaml.

With --server, the token is stored on the context named after that server's
hostname, creating it if none exists, and that context becomes current.
Without --server, the token is stored on the current context (a context is
created from the default server if the config is empty).

With --token, the given token is stored directly. Without --token, an OAuth 2.0
device authorization flow is run against the server's Keycloak: rossoctl reads
the Keycloak URL, realm, and client id from GET <server>/auth/config, shows a
verification URL and one-time code (and attempts to open a browser), then polls
until you authorize.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}

		// Determine which context to log into:
		//   - With an explicit --server, target the context named after that
		//     server's hostname, creating it if none exists, and make it
		//     current.
		//   - Without --server, log into the current context (loadConfig has
		//     already seeded one if the config was empty).
		var target *config.Context
		if cmd.Flags().Changed("server") {
			name := config.ContextNameForServer(server)
			if existing, ok := cfg.Get(name); ok {
				target = existing
			} else {
				cfg.Upsert(config.Context{Name: name, Type: config.TypeAPI, Server: server})
				target, _ = cfg.Get(name)
			}
			if err := cfg.SetCurrent(name); err != nil {
				return err
			}
		} else {
			cur, ok := cfg.Current()
			if !ok {
				// loadConfig seeds a current context, so this should not happen.
				return fmt.Errorf("no current context to log in to")
			}
			target = cur
		}

		token := loginToken
		if token == "" {
			// Authentication enablement is checked here, not in deviceLogin: the
			// decision to run the device flow at all belongs to the caller, and
			// only this path needs the server's auth config.
			authCfg, err := fetchAuthConfigForLogin(cmd)
			if err != nil {
				return err
			}

			// Without auth there is no flow to run and no token to obtain, so
			// proceeding would store an empty one and report success. Say so
			// instead, and name the way to log in to such a server.
			if !authCfg.Enabled {
				return fmt.Errorf("authentication is not enabled on the server; use --token instead")
			}

			token, err = deviceLogin(cmd, authCfg)
			if err != nil {
				return err
			}
		}

		target.BearerToken = token

		// If the context has no namespace yet, pick the first one the server
		// reports (using the token we just obtained). This is best-effort: a
		// failure to list, or a server with no namespaces, leaves the namespace
		// blank without failing the login.
		if target.Namespace == "" {
			if ns := firstNamespace(cmd, target); ns != "" {
				target.Namespace = ns
			}
		}

		if err := cfg.Save(); err != nil {
			return err
		}

		if target.Namespace != "" {
			cmd.Printf("Logged in; token set on context %q (namespace %q).\n", target.Name, target.Namespace)
		} else {
			cmd.Printf("Logged in; token set on context %q.\n", target.Name)
		}
		// Named here because the token's roles and audiences are what decide
		// which operations will now succeed, and this is the moment the user has
		// them: `auth status` decodes the token just stored.
		cmd.Println("Run `rossoctl auth status` to see the privileges this token carries.")
		return nil
	},
}

// firstNamespace returns the first namespace the target context's server
// reports, or "" if it cannot be determined (list error or none available).
// It builds its own client from the target so the just-obtained token is used
// regardless of how the effective server would otherwise resolve.
func firstNamespace(cmd *cobra.Command, target *config.Context) string {
	client := rossoctlclient.NewClient(target)
	attachVerboseLogger(cmd, client)
	resp, err := client.ListNamespaces(cmd.Context(), true)
	if err != nil || len(resp.Namespaces) == 0 {
		return ""
	}
	return resp.Namespaces[0]
}

// fetchAuthConfigForLogin reads the resolved server's auth config, which the
// device flow needs for its Keycloak details.
//
// It is separate from the flow itself so the caller can decide, from
// authCfg.Enabled, whether to start the flow at all.
func fetchAuthConfigForLogin(cmd *cobra.Command) (*apiclient.AuthConfig, error) {
	client, err := newClient(cmd)
	if err != nil {
		return nil, err
	}
	authCfg, err := client.GetAuthConfig(cmd.Context())
	if err != nil {
		// Reported as its own failure rather than passed through, so the 401
		// hint does not fire here: this *is* login, and telling the user to run
		// it would send them in a circle. /auth/config is unauthenticated, so a
		// 401 from it is a server or proxy problem, not a missing credential.
		return nil, fmt.Errorf("reading auth config from the server, needed to start the login flow: %v", err)
	}
	return authCfg, nil
}

// deviceLogin runs the OAuth device authorization flow against the Keycloak
// named by authCfg and returns the resulting access token.
//
// The caller supplies authCfg and is responsible for having checked that
// authentication is enabled; deviceLogin reads only the Keycloak details from
// it and does not revisit that decision.
func deviceLogin(cmd *cobra.Command, authCfg *apiclient.AuthConfig) (string, error) {
	kcURL := deref(authCfg.KeycloakURL)
	realm := deref(authCfg.Realm)
	clientID := deref(authCfg.ClientID)
	if kcURL == "-" || realm == "-" || clientID == "-" {
		return "", fmt.Errorf("server auth config is missing keycloak_url, realm, or client_id")
	}

	df := &deviceflow.Client{
		KeycloakURL: kcURL,
		Realm:       realm,
		ClientID:    clientID,
		Sleep:       deviceflowSleep,
	}
	if verbose {
		errOut := cmd.ErrOrStderr()
		df.Logf = func(format string, args ...any) {
			fmt.Fprintf(errOut, format+"\n", args...)
		}
	}

	da, err := df.RequestDeviceCode(cmd.Context())
	if err != nil {
		return "", err
	}

	// Prompt on stderr so stdout stays clean, then best-effort open a browser.
	errOut := cmd.ErrOrStderr()
	fmt.Fprintf(errOut, "To sign in, visit:\n  %s\nand enter code: %s\n",
		da.VerificationURI, da.UserCode)

	openURL := da.VerificationURIComplete
	if openURL == "" {
		openURL = da.VerificationURI
	}
	if err := browserOpener(openURL); err == nil {
		fmt.Fprintf(errOut, "(opened %s in your browser)\n", openURL)
	}
	fmt.Fprintln(errOut, "Waiting for authorization...")

	return df.PollToken(cmd.Context(), da)
}

// browserOpener opens a URL in the user's browser. It is a variable so tests
// can replace it with a no-op, avoiding spawning a real browser process.
var browserOpener = openBrowser

// deviceflowSleep is the wait between device-flow token polls. It is a
// variable (nil => the deviceflow package uses time.Sleep) so tests can
// replace it with a no-op to avoid real delays.
var deviceflowSleep func(d time.Duration)

// openBrowser attempts to open url in the user's default browser. It is
// best-effort: any error (including an unsupported platform) is returned for
// the caller to ignore.
func openBrowser(url string) error {
	var name string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default: // linux, bsd, etc.
		name = "xdg-open"
	}
	args = append(args, url)
	return exec.Command(name, args...).Start()
}

func init() {
	loginCmd.Flags().StringVar(&loginToken, "token", "", "bearer token to store on the current context; if omitted, run the device login flow")
	rootCmd.AddCommand(loginCmd)
}
