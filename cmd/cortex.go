package cmd

// cortexName is the name of the cortex to operate on, bound to the cortex
// group's --cortex flag.
var cortexName string

// defaultCortexName is the cortex name used when --cortex is not given.
const defaultCortexName = "default"

func init() {
	cortexCmd := newGroup("cortex", "Manage cortexes")

	// Persistent so every cortex subcommand inherits it.
	cortexCmd.PersistentFlags().StringVar(&cortexName, "cortex", defaultCortexName,
		"name of the cortex to operate on")

	cortexCmd.AddCommand(cortexServeCmd)
	rootCmd.AddCommand(cortexCmd)
}
