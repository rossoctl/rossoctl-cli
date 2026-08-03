//go:build !exclude_plugin_a2aparser

package cmd

// Registering a2aparser in the command package (not authlib) mirrors the authbridge
// binaries: each plugin is a blank import in its own build-tag-excludable file,
// so a build can drop one with - exclude_plugin_a2aparser without touching code.
// Without these imports the plugin registry is empty and any config naming a
// plugin fails with "unknown plugin".
import _ "github.com/rossoctl/cortex/authbridge/authlib/plugins/a2aparser"
