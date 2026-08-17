package cmd

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

// toolsImportDeploymentType backs the persistent --deployment-type flag on the
// tools import group, inherited by from-image and from-source. It maps to the
// backend's workloadType.
var toolsImportDeploymentType string

// toolsImportCreateHTTPRoute backs the persistent --createHttpRoute flag on the
// tools import group, inherited by from-image and from-source. It maps to the
// backend's createHttpRoute.
//
// Persistent, like --deployment-type, rather than declared on from-image alone:
// exposing the tool is not specific to how it was built, so a flag that worked
// on one subcommand and was rejected by the other would be a difference with no
// reason behind it.
var toolsImportCreateHTTPRoute bool

// toolsImportAdditionalParameterJSON backs the persistent, repeatable
// --additionalParameterJSON flag on the tools import group, mirroring the agents
// one; see importAdditionalParameterJSON in agents_import.go for why it is
// persistent and a nil-defaulted StringArray.
var toolsImportAdditionalParameterJSON []string

// newToolsImportCmd builds the `tools import` command and its two subcommands,
// `from-image` and `from-source`, mirroring `agents import`.
//
// The namespace for the created tool comes from the tools group's --namespace
// flag (or the current context), via toolsNamespace().
func newToolsImportCmd() *cobra.Command {
	importCmd := newGroup("import", "Import a tool from an image or from source")

	// Persistent so both subcommands inherit them. Tools support deployment and
	// statefulset workload types.
	importCmd.PersistentFlags().StringVar(&toolsImportDeploymentType, "deployment-type", "deployment",
		"workload type for the tool: deployment|statefulset")
	importCmd.PersistentFlags().BoolVar(&toolsImportCreateHTTPRoute, "createHttpRoute", false,
		"create an HTTPRoute exposing the tool")
	importCmd.PersistentFlags().StringArrayVar(&toolsImportAdditionalParameterJSON, additionalParameterFlagName, nil,
		"JSON dict, or a file containing one, merged into the request body (repeatable; later values and these keys win)")

	importCmd.AddCommand(
		newToolsImportFromImageCmd(),
		newToolsImportFromSourceCmd(),
	)
	return importCmd
}

// defaultToolPort is the --ports default: a TCP port named "http" on 9090.
var defaultToolPorts = []string{"http:9090:9090:TCP"}

func newToolsImportFromImageCmd() *cobra.Command {
	var (
		name            string
		envVarsURL      string
		envVarFlags     []string
		containerImage  string
		imagePullSecret string
		ports           []string
	)

	cmd := &cobra.Command{
		Use:   "from-image",
		Short: "Import a tool from an existing container image",
		Long: `Import a tool from an existing container image (POST <server>/tools).

The tool is created in the namespace from the tools --namespace flag, or the
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

			namespace, err := toolsNamespace()
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

			servicePorts, err := parseServicePorts(ports)
			if err != nil {
				return err
			}

			// Before newClient, like the parsing above: a malformed dict or an
			// unreadable file must fail without having created the tool.
			additional, err := loadAdditionalParameters(toolsImportAdditionalParameterJSON)
			if err != nil {
				return err
			}

			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			resp, err := client.CreateTool(cmd.Context(), &apiclient.CreateToolRequest{
				Name:             name,
				Namespace:        namespace,
				DeploymentMethod: "image",
				WorkloadType:     toolsImportDeploymentType,
				ContainerImage:   containerImage,
				ImagePullSecret:  imagePullSecret,
				EnvVars:          envVars,
				ServicePorts:     servicePorts,
				CreateHTTPRoute:  toolsImportCreateHTTPRoute,

				// Applied when the request is marshaled, so it wins over every field
				// above.
				AdditionalParameters: additional,
			})
			if err != nil {
				return err
			}

			printCreateToolResult(cmd, resp, name, namespace)
			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&name, "name", "", "name of the tool (required)")
	f.StringVar(&envVarsURL, "envVarsURL", "", "URL to fetch environment variables from (newline-separated key=value)")
	// StringArrayVar rather than the StringSliceVar used by --ports below, and the
	// default must stay nil: see the note in agents_import.go. In short, an env
	// value may legitimately contain commas or quotes, which a StringSlice would
	// split or reject, while a port spec never does.
	f.StringArrayVar(&envVarFlags, "envVar", nil,
		"environment variable as key=value (repeatable; wins over --envVarsURL for the same name)")
	f.StringVar(&containerImage, "containerImage", "", "container image to deploy (required)")
	f.StringVar(&imagePullSecret, "imagePullSecret", "", "name of the image pull secret")
	f.StringSliceVar(&ports, "ports", defaultToolPorts,
		`service ports as name:port:targetPort[:protocol] (repeatable or comma-separated); a bare "port" means http:port:port:TCP`)

	return cmd
}

// parseServicePorts converts --ports entries into CreateServicePorts. Each
// entry is "name:port:targetPort[:protocol]"; a bare "port" is shorthand for
// name "http", targetPort equal to port, protocol TCP. protocol defaults to
// TCP when omitted.
func parseServicePorts(specs []string) ([]apiclient.CreateServicePort, error) {
	var out []apiclient.CreateServicePort
	for _, spec := range specs {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			continue
		}
		fields := strings.Split(spec, ":")

		sp := apiclient.CreateServicePort{Name: "http", Protocol: "TCP"}
		switch len(fields) {
		case 1:
			// "port" -> http:port:port:TCP
			p, err := strconv.Atoi(fields[0])
			if err != nil {
				return nil, fmt.Errorf("invalid --ports %q: port must be an integer", spec)
			}
			sp.Port, sp.TargetPort = p, p
		case 2, 3, 4:
			sp.Name = fields[0]
			if sp.Name == "" {
				return nil, fmt.Errorf("invalid --ports %q: name must not be empty", spec)
			}
			p, err := strconv.Atoi(fields[1])
			if err != nil {
				return nil, fmt.Errorf("invalid --ports %q: port must be an integer", spec)
			}
			sp.Port = p
			sp.TargetPort = p
			if len(fields) >= 3 {
				tp, err := strconv.Atoi(fields[2])
				if err != nil {
					return nil, fmt.Errorf("invalid --ports %q: targetPort must be an integer", spec)
				}
				sp.TargetPort = tp
			}
			if len(fields) == 4 {
				if fields[3] == "" {
					return nil, fmt.Errorf("invalid --ports %q: protocol must not be empty", spec)
				}
				sp.Protocol = fields[3]
			}
		default:
			return nil, fmt.Errorf("invalid --ports %q: expected name:port:targetPort[:protocol]", spec)
		}
		out = append(out, sp)
	}
	return out, nil
}

func newToolsImportFromSourceCmd() *cobra.Command {
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
		Short: unimplementedPrefix + "Import a tool by building from source",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return unimplementedRunE(cmd, nil)
		},
	}

	f := cmd.Flags()
	f.StringVar(&name, "name", "", "name of the tool")
	f.StringVar(&envVarsURL, "envVarsURL", "", "URL to fetch environment variables from")
	// Declared for parity with --envVarsURL above and unread for the same reason:
	// this subcommand is a stub.
	f.StringArrayVar(&envVarFlags, "envVar", nil, "environment variable as key=value (repeatable)")
	f.StringVar(&gitURL, "gitUrl", "", "git repository URL to build from")
	f.StringVar(&gitPath, "gitPath", "", "path within the git repository")
	f.StringVar(&gitBranch, "gitBranch", "main", "git branch to build from")

	return cmd
}

// printCreateToolResult reports the outcome of a create request, preferring
// the server's message.
func printCreateToolResult(cmd *cobra.Command, resp *apiclient.CreateToolResponse, name, namespace string) {
	if resp.Message != "" {
		cmd.Println(resp.Message)
		return
	}
	cmd.Printf("Tool %q created in namespace %q.\n", name, namespace)
}
