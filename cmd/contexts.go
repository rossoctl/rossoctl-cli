package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
)

var contextsNamespace string

func contextListError(err error) error {
	var statusErr *apiclient.StatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound {
		return fmt.Errorf("this Rosso server does not support context infrastructure; context commands require the context resource API introduced by rossoctl/rossoctl#2392: %w", err)
	}
	return err
}

func contextNamespace() (string, error) {
	if contextsNamespace != "" {
		return contextsNamespace, nil
	}
	return currentNamespace()
}

func newContextsCreateCmd() *cobra.Command {
	var contextType, backend, size, storageClass string
	var shared, jsonOutput bool
	cmd := &cobra.Command{
		Use:   "create NAME",
		Short: "Create a named context resource",
		Long: `Create a named context resource.

Types classify how the stored data is intended to be used:
  workspace   Mutable files used while an agent works
  memory      Durable observations and experiences
  knowledge   Synthesized, reusable understanding
  artifacts   Produced reports, media, and other outputs

All types currently use the same PVC-backed storage and lifecycle behavior.`,
		Example: `  # Create a 1Gi ReadWriteOnce workspace
  rossoctl context create research

  # Create PVC-backed memory for an agent
  rossoctl context create research-memory --type memory --size 5Gi

  # Create a 10Gi shared ReadWriteMany workspace on a storage class
  rossoctl context create research-shared --shared --size 10Gi --storage-class ibm-scale-csi

  # Mount the context when importing a Sandbox agent
  rossoctl agents import --deployment-type sandbox --context research:/workspace from-image --name agent-1 --containerImage IMAGE`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			namespace, err := contextNamespace()
			if err != nil {
				return err
			}
			mode := "ReadWriteOnce"
			if shared {
				mode = "ReadWriteMany"
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			result, err := client.CreateContext(cmd.Context(), &apiclient.CreateContextRequest{
				Name: args[0], Namespace: namespace, Type: contextType,
				Storage: apiclient.ContextStorage{
					Backend: backend, Size: size, AccessMode: mode, StorageClass: storageClass,
				},
			})
			if err != nil {
				return err
			}
			return printContextResource(cmd, result, jsonOutput)
		},
	}
	cmd.Flags().StringVar(&contextType, "type", "workspace", "context type (workspace, memory, knowledge, or artifacts)")
	cmd.Flags().StringVar(&backend, "backend", "pvc", "storage backend (currently pvc)")
	cmd.Flags().StringVarP(&size, "size", "s", "1Gi", "storage size")
	cmd.Flags().StringVar(&storageClass, "storage-class", "", "Kubernetes storage class")
	cmd.Flags().BoolVar(&shared, "shared", false, "use shared ReadWriteMany storage")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON")
	return cmd
}

func newContextsGetCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use: "get NAME", Short: "Show a context resource", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			namespace, err := contextNamespace()
			if err != nil {
				return err
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			result, err := client.GetContext(cmd.Context(), namespace, args[0])
			if err != nil {
				return err
			}
			return printContextResource(cmd, result, jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON")
	return cmd
}

func newContextsListCmd() *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List context resources",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			namespace, err := contextNamespace()
			if err != nil {
				return err
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			result, err := client.ListContexts(cmd.Context(), namespace)
			if err != nil {
				return contextListError(err)
			}
			if jsonOutput {
				encoded, err := json.MarshalIndent(result.Items, "", "  ")
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
				return nil
			}
			if len(result.Items) == 0 {
				cmd.Println("No contexts found.")
				return nil
			}
			writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(writer, "NAME\tTYPE\tSTATUS\tSIZE\tACCESS MODE\tCLAIM")
			for _, item := range result.Items {
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", item.Name, item.Type, item.Status,
					item.Storage.Size, item.Storage.AccessMode, item.Attachment.ClaimName)
			}
			return writer.Flush()
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "print JSON")
	return cmd
}

func newContextsDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use: "delete NAME", Aliases: []string{"rm"}, Short: "Delete a context resource", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			namespace, err := contextNamespace()
			if err != nil {
				return err
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			if err := client.DeleteContext(cmd.Context(), namespace, args[0]); err != nil {
				return err
			}
			cmd.Printf("Context %q deleted from namespace %q.\n", args[0], namespace)
			return nil
		},
	}
}

func printContextResource(cmd *cobra.Command, value *apiclient.ContextResource, jsonOutput bool) error {
	if jsonOutput {
		encoded, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(encoded))
		return nil
	}
	storageClass := value.Storage.StorageClass
	if storageClass == "" {
		storageClass = "<default>"
	}
	writer := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "Context Information")
	fmt.Fprintf(writer, "  Name:\t%s\n", value.Name)
	fmt.Fprintf(writer, "  Namespace:\t%s\n", value.Namespace)
	fmt.Fprintf(writer, "  Type:\t%s\n", value.Type)
	fmt.Fprintf(writer, "  Status:\t%s\n", value.Status)
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "Storage")
	fmt.Fprintf(writer, "  Backend:\t%s\n", value.Storage.Backend)
	fmt.Fprintf(writer, "  Size:\t%s\n", value.Storage.Size)
	fmt.Fprintf(writer, "  Access Mode:\t%s\n", value.Storage.AccessMode)
	fmt.Fprintf(writer, "  Storage Class:\t%s\n", storageClass)
	fmt.Fprintln(writer)
	fmt.Fprintln(writer, "Attachment")
	fmt.Fprintf(writer, "  Kind:\t%s\n", value.Attachment.Kind)
	fmt.Fprintf(writer, "  Claim:\t%s\n", value.Attachment.ClaimName)
	return writer.Flush()
}

func init() {
	contextsCmd := newGroup("contexts", "Manage durable context infrastructure for agents")
	contextsCmd.Aliases = []string{"context"}
	contextsCmd.Long = `Manage durable context infrastructure for agents.

Context resources make files available to agents as workspaces, memory,
knowledge, or artifacts. They are distinct from rossoctl configuration
contexts and from an LLM's finite context window. The current backend is
PVC-backed storage mounted into StatefulSet or Sandbox agents.

Learn more:
https://github.com/rossoctl/rossoctl/blob/main/docs/concepts/context-service.md`
	contextsCmd.PersistentFlags().StringVar(&contextsNamespace, "namespace", "", "namespace (overrides current context)")
	contextsCmd.AddCommand(newContextsCreateCmd(), newContextsListCmd(), newContextsGetCmd(), newContextsDeleteCmd())
	rootCmd.AddCommand(contextsCmd)
}
