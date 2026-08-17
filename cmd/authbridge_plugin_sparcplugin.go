//go:build !exclude_plugin_sparc

package cmd

// Registering sparc in the command package (not authlib) mirrors the authbridge
// binaries: each plugin is a blank import in its own build-tag-excludable file,
// so a build can drop one with -tags exclude_plugin_sparc without touching code.
// Without these imports the plugin registry is empty and any config naming a
// plugin fails with "unknown plugin".
//
// The file name ends in "sparcplugin", not "sparc", and must keep doing so.
// "sparc" is one of the architecture names Go recognizes in the implicit
// filename constraint, so a file called authbridge_plugin_sparc.go is
// constrained to GOARCH=sparc and silently dropped everywhere else: it lands in
// `go list`'s IgnoredGoFiles, this import never runs, and a config naming the
// plugin fails with `unknown plugin "sparc"` on amd64 and arm64 alike. The
// build tag above cannot save it, because the file name is applied first. The
// same suffix is used by cortex's own cmd/authbridge-proxy/plugins_sparcplugin.go.
//
// Renaming this file to match its siblings exactly would reintroduce that bug
// (rossoctl-cli#68), and nothing would fail at build time to say so.
import _ "github.com/rossoctl/cortex/authbridge/authlib/plugins/sparc"
