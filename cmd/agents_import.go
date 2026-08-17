package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

// importDeploymentType backs the persistent --deployment-type flag on the
// import group, inherited by from-image and from-source. It maps to the
// backend's workloadType.
var importDeploymentType string

// importCreateHTTPRoute backs the persistent --createHttpRoute flag on the
// import group, inherited by from-image and from-source. It maps to the
// backend's createHttpRoute.
//
// Persistent, like --deployment-type, rather than declared on from-image alone:
// exposing the agent is not specific to how it was built, so a flag that worked
// on one subcommand and was rejected by the other would be a difference with no
// reason behind it.
var importCreateHTTPRoute bool

// importAdditionalParameterJSON backs the persistent, repeatable
// --additionalParameterJSON flag on the import group. Each value is a JSON dict
// or the name of a file containing one; all of them are merged and overlaid onto
// the request body. See loadAdditionalParameters.
//
// Persistent, like the two flags above, because reaching a backend field the CLI
// has no flag for is not specific to how the agent was built.
//
// A StringArray with a nil default, for the reasons given for --envVar below: a
// JSON document routinely contains commas and quotes, which a StringSlice would
// split or reject outright, and a non-nil slice default leaks between tests.
var importAdditionalParameterJSON []string

// newAgentsImportCmd builds the `agents import` command and its two
// subcommands, `from-image` and `from-source`.
//
// The namespace for the created agent comes from the agents group's
// --namespace flag (or the current context), via agentsNamespace().
func newAgentsImportCmd() *cobra.Command {
	importCmd := newGroup("import", "Import an agent from an image or from source")

	// Persistent so both subcommands inherit them.
	importCmd.PersistentFlags().StringVar(&importDeploymentType, "deployment-type", "deployment",
		"workload type for the agent: deployment|statefulset|job|sandbox")
	importCmd.PersistentFlags().BoolVar(&importCreateHTTPRoute, "createHttpRoute", false,
		"create an HTTPRoute exposing the agent")
	importCmd.PersistentFlags().StringArrayVar(&importAdditionalParameterJSON, additionalParameterFlagName, nil,
		"JSON dict, or a file containing one, merged into the request body (repeatable; later values and these keys win)")

	importCmd.AddCommand(
		newAgentsImportFromImageCmd(),
		newAgentsImportFromSourceCmd(),
	)
	return importCmd
}

func newAgentsImportFromImageCmd() *cobra.Command {
	var (
		name            string
		envVarsURL      string
		envVarFlags     []string
		containerImage  string
		imagePullSecret string
		storageSize     string
	)

	cmd := &cobra.Command{
		Use:   "from-image",
		Short: "Import an agent from an existing container image",
		Long: `Import an agent from an existing container image (POST <server>/agents).

The agent is created in the namespace from the agents --namespace flag, or the
current context's namespace. --deployment-type selects the workload type.

Env vars come from --envVarsURL (a document of newline-separated key=value
pairs) and from --envVar key=value, which may be repeated. When both name the
same variable, --envVar wins, whatever order the flags appear in.

--additionalParameterJSON sends request fields this command has no flag for. Its
value is either a JSON dict or the name of a file containing one, and the flag
may be repeated; all of the dicts are merged, a later one winning for a key they
share, and the result is overlaid onto the request body. A key that names a field
the flags above already set replaces it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			if containerImage == "" {
				return fmt.Errorf("--containerImage is required")
			}
			if storageSize != "" && importDeploymentType != "statefulset" && importDeploymentType != "sandbox" {
				return fmt.Errorf("--storage-size requires --deployment-type statefulset or sandbox")
			}

			namespace, err := agentsNamespace()
			if err != nil {
				return err
			}

			docEnvVars, err := fetchEnvVars(cmd.Context(), cmd, envVarsURL)
			if err != nil {
				return err
			}
			flagEnvVars, err := parseEnvVarFlags(envVarFlags)
			if err != nil {
				return err
			}
			// Document first, flags second: an explicit --envVar wins over the
			// fetched document for the same name.
			envVars := mergeEnvVars(docEnvVars, flagEnvVars)

			// Before newClient, like the env-var parsing above: a malformed dict or
			// an unreadable file must fail without having created the agent.
			additional, err := loadAdditionalParameters(importAdditionalParameterJSON)
			if err != nil {
				return err
			}

			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			request := &apiclient.CreateAgentRequest{
				Name:             name,
				Namespace:        namespace,
				DeploymentMethod: "image",
				WorkloadType:     importDeploymentType,
				ContainerImage:   containerImage,
				ImagePullSecret:  imagePullSecret,
				EnvVars:          envVars,
				CreateHTTPRoute:  importCreateHTTPRoute,

				// Set last, but applied last as well: the overlay happens when the
				// request is marshaled, so it wins over every field above — including
				// PersistentStorage, assigned after this literal.
				AdditionalParameters: additional,
			}
			if storageSize != "" {
				request.PersistentStorage = &apiclient.PersistentStorageConfig{
					Enabled: true,
					Size:    storageSize,
				}
			}
			resp, err := client.CreateAgent(cmd.Context(), request)
			if err != nil {
				return err
			}

			printCreateAgentResult(cmd, resp, name, namespace)
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&name, "name", "", "name of the agent (required)")
	f.StringVar(&envVarsURL, "envVarsURL", "", "URL to fetch environment variables from (newline-separated key=value)")
	// StringArrayVar, not the StringSliceVar used elsewhere in this package: a
	// StringSlice splits each value as CSV, which mangles legitimate env values
	// and rejects others outright. "--envVar TAGS=a,b,c" would arrive as TAGS=a
	// plus two bare "b" and "c" entries, which then fail key=value parsing with an
	// error naming text the user never typed, and '--envVar {"a":1}' would fail in
	// the flag layer with `bare " in non-quoted-field`. A StringArray keeps every
	// value literal, so only repeating the flag adds an entry.
	//
	// The default must stay nil. resetFlags in root_test.go restores slice
	// defaults through pflag's Replace, which does not clear pflag's private
	// "changed" bit, so the first Set in a later test appends instead of
	// replacing; appending to a nil default is still correct, while a non-nil one
	// would leak between tests.
	f.StringArrayVar(&envVarFlags, "envVar", nil,
		"environment variable as key=value (repeatable; wins over --envVarsURL for the same name)")
	f.StringVar(&containerImage, "containerImage", "", "container image to deploy (required)")
	f.StringVar(&imagePullSecret, "imagePullSecret", "", "name of the image pull secret")
	f.StringVar(&storageSize, "storage-size", "", "enable persistent storage with this size (statefulset or sandbox, for example 5Gi)")

	return cmd
}

func newAgentsImportFromSourceCmd() *cobra.Command {
	var (
		name        string
		envVarsURL  string
		envVarFlags []string
		gitURL      string
		gitPath     string
		gitBranch   string
	)

	cmd := &cobra.Command{
		Use:   "from-source",
		Short: unimplementedPrefix + "Import an agent by building from source",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return unimplementedRunE(cmd, nil)
		},
	}

	f := cmd.Flags()
	f.StringVar(&name, "name", "", "name of the agent")
	f.StringVar(&envVarsURL, "envVarsURL", "", "URL to fetch environment variables from")
	// Declared for parity with --envVarsURL above and unread for the same reason:
	// this subcommand is a stub, so whoever implements it finds both flags in
	// place rather than a half-built surface.
	f.StringArrayVar(&envVarFlags, "envVar", nil, "environment variable as key=value (repeatable)")
	f.StringVar(&gitURL, "gitUrl", "", "git repository URL to build from")
	f.StringVar(&gitPath, "gitPath", "", "path within the git repository")
	f.StringVar(&gitBranch, "gitBranch", "main", "git branch to build from")

	return cmd
}

// printCreateAgentResult reports the outcome of a create request, preferring
// the server's message.
func printCreateAgentResult(cmd *cobra.Command, resp *apiclient.CreateAgentResponse, name, namespace string) {
	if resp.Message != "" {
		cmd.Println(resp.Message)
		return
	}
	cmd.Printf("Agent %q created in namespace %q.\n", name, namespace)
}
