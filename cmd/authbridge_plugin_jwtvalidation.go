//go:build !exclude_plugin_jwtvalidation

package cmd

// Registering jwtvalidation in the command package (not authlib) mirrors the authbridge
// binaries: each plugin is a blank import in its own build-tag-excludable file,
// so a build can drop one with -tags exclude_plugin_jwtvalidation without touching code.
// Without these imports the plugin registry is empty and any config naming a
// plugin fails with "unknown plugin".
import _ "github.com/rossoctl/cortex/authbridge/authlib/plugins/jwtvalidation"
