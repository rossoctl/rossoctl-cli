// Package buildinfo holds version metadata for the CLI.
//
// It lives under internal/ so only this module can import it, and it has no
// dependency on Cobra — the values are injected by the cmd package (which in
// turn receives them from -ldflags at build time). Keeping it decoupled makes
// the formatting logic trivially unit-testable.
package buildinfo

import "fmt"

// Version is the CLI's build version, set at build time via -ldflags (see the
// Makefile). It lives here, rather than in cmd, so that non-Cobra packages
// such as internal/serve can report it without importing cmd.
var Version = "dev"

// Info describes how the binary was built.
type Info struct {
	Version string
	Commit  string
	Date    string
}

// String renders the build information as a single human-readable line.
func (i Info) String() string {
	return fmt.Sprintf("rossoctl %s (commit %s, built %s)", i.Version, i.Commit, i.Date)
}
