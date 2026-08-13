package cmd

import (
	"github.com/spf13/cobra"

	"github.com/rossoctl/rossoctl-cli/internal/config"
)

// ensureCortexContext creates the cortex context when it is absent, and makes it
// current when makeCurrent is set.
//
// Both commands that start a local cortex call this so that the context naming it
// exists without the user having to run `config create-context` or
// `login --cortex` first: having started a cortex, they are plainly working with
// one, and the context is what routes a later `agents list` to it.
//
// makeCurrent is a parameter rather than always true because the two callers
// differ on exactly that point, and only that point:
//
//   - `cortex serve` switches. Serving a local cortex in the foreground is a
//     statement about what the user is working on, and the command holds the
//     terminal until interrupted, so the switch is neither a surprise nor
//     something that happens behind an unrelated task.
//   - `authbridge exec` does not. It hosts an arbitrary command behind a
//     pipeline and reads nothing from the context — no server, token, or
//     namespace — so switching would repoint every later rossoctl invocation as
//     a side effect of running something unrelated. That side effect was a bug
//     once already; see runCortexExec.
//
// Creating the context is safe in both cases because it is additive: it changes
// where nothing points, only what is available to point at.
//
// Best-effort by design — the returned error is for the caller to report or drop.
// Neither caller's real work needs the config, so a read-only config directory
// should not stop a server from serving or a command from running.
func ensureCortexContext(cmd *cobra.Command, makeCurrent bool) error {
	// loadConfigReadOnly, not loadConfig: loadConfig seeds the whole default
	// context set on first use, including one pointing at the default *remote* API
	// server, and elects it current. Starting a local cortex is no reason to
	// configure a remote server, and for exec it would reintroduce exactly the
	// silent repointing this helper is careful to avoid. Here the only context that
	// ever comes into existence is cortex.
	cfg, err := loadConfigReadOnly()
	if err != nil {
		return err
	}

	target, ok := cfg.Get(config.CortexContextName)
	if !ok {
		cfg.Upsert(config.CortexContext())
		target, _ = cfg.Get(config.CortexContextName)
	} else if !makeCurrent && target.Namespace != "" {
		// Already present and nothing to change: return before Save so an existing
		// config is not rewritten on every exec.
		return nil
	}

	if makeCurrent {
		if err := cfg.SetCurrent(target.Name); err != nil {
			return err
		}
	}

	// Same best-effort namespace fill as `login --cortex`: a cortex context with
	// no namespace is rejected before any request is built, and the namespaces
	// come from the local instance records rather than from a server. A machine
	// where nothing has run yet simply has none to offer.
	if target.Namespace == "" {
		if ns := firstNamespace(cmd, target); ns != "" {
			target.Namespace = ns
		}
	}

	return cfg.Save()
}

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
contacting a server.`

	cortexCmd.AddCommand(cortexServeCmd)
	rootCmd.AddCommand(cortexCmd)
}
