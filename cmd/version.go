package cmd

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/buildinfo"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the version information",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		info := buildinfo.Info{Version: buildinfo.Version, Commit: commit, Date: date}
		cmd.Println(info.String())
		cmd.Println("server version: " + serverVersion(cmd))
		return nil
	},
}

// serverVersion reports the connected server's version, as returned by
// GET /auth/config. It returns "unknown" whenever the server is unreachable
// or doesn't report a version, so `version` never fails just because the
// server is down.
func serverVersion(cmd *cobra.Command) string {
	client, err := newClient(cmd)
	if err != nil {
		return "unknown"
	}
	cfg, err := client.GetAuthConfig(cmd.Context())
	if err != nil || cfg.Version == nil || strings.TrimSpace(*cfg.Version) == "" {
		return "unknown"
	}
	return *cfg.Version
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
