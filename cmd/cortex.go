package cmd

// cortexName is the name of the cortex to operate on, bound to the cortex
// group's --cortex flag.
var cortexName string

// defaultCortexName is the cortex name used when --cortex is not given.
const defaultCortexName = "default"

func init() {
	cortexCmd := newGroup("cortex", "Manage cortexes")

	// newGroup supplies only Use and Short, so the long help is set here. It
	// documents the naming rule because that rule is what decides where a
	// command's request goes, and nothing in a context's stored fields reveals
	// it.
	cortexCmd.Long = `Manage cortexes.

A cortex is a local rossoctl backend, serving the agents and tools that
"authbridge exec" has started on this machine.

rossoctl commands reach one by the name of the context: a context named
"cortex" is answered by the cortex handlers inside the command's own process,
reading the instance records directly, so no server has to be running.

  rossoctl config create-context --name cortex \
      --server http://localhost:9097/api/v1/ --namespace team1
  rossoctl agents list

The name is the whole rule, so a context named "cortex" whose server points at
a remote machine is answered locally rather than there. Run any command with
--verbose to see which transport answered it.

"cortex serve" starts a real HTTP server for the cases this in-process path
cannot cover — pointing a web UI, or anything else that is not rossoctl, at a
local cortex.

"rossoctl login --cortex" creates that context and makes it current without
contacting a server. Note that its --cortex is a boolean, unlike this group's
--cortex, which names the cortex to operate on.`

	// Persistent so every cortex subcommand inherits it.
	cortexCmd.PersistentFlags().StringVar(&cortexName, "cortex", defaultCortexName,
		"name of the cortex to operate on")

	cortexCmd.AddCommand(cortexServeCmd)
	rootCmd.AddCommand(cortexCmd)
}
