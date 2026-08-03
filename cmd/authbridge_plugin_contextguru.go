//go:build !exclude_plugin_contextguru

// context-guru is compiled in by default, like the other plugins, and dropped
// with -tags exclude_plugin_contextguru. Note this differs from the authbridge
// binaries, where it is opt-IN via include_plugin_contextguru: its embedded
// engine pulls a large transitive set (bifrost/core, tiktoken-go, tree-sitter
// grammars, starlark), which those binaries keep out of the default image. The
// rossoctl CLI takes the binary-size cost so `authbridge exec` runs the context-guru
// demos without a special build.
package cmd

import _ "github.com/rossoctl/cortex/authbridge/authlib/plugins/contextguru"
