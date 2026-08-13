# rossoctl-cli

A command-line interface for Rossoctl, built with [Cobra](https://github.com/spf13/cobra).

## Install

The `downloadRossoctl` script downloads the release archive for your platform
extracts it, and installs the binary at
`$HOME/.config/rossoctl/rossoctl`:

```sh
curl -fsSL https://raw.githubusercontent.com/rossoctl/rossoctl-cli/main/downloadRossoctl | sh
PATH=$PATH:$HOME/.config/rossoctl
# alternately, sudo mv $HOME/.config/rossoctl /usr/local/bin
```

## Quick usage, for shared OpenShift Rossoctl API servers

```sh
# (Choose w3id for shared cluster login)
rossoctl --server https://rossoctl-ui-rossoctl-system.apps.ykt3.hcp.res.ibm.com/api/v1 login
rossoctl agents list
```

## Quick usage, for existing Kind cluster Rossoctl API server

```sh
rossoctl login
rossoctl agents list
```

## Running a command behind an AuthBridge pipeline

```sh
# `--config` is required and takes a local YAML file or a URL serving YAML (a
# remote config is fetched to a temp file, which is removed on exit). Everything
# after `--` is passed through to the command untouched, and rossoctl exits with
# the command's exit status.
rossoctl authbridge exec --config ./authbridge.yaml -- claude "explain this repo"
rossoctl authbridge exec --config https://example.com/authbridge.yaml -- ./script.sh --verbose
```

What `authbridge exec` starts, driven by the config:

| Config | What starts |
| --- | --- |
| `listener.roles` includes `forward` | forward proxy on `listener.forward_proxy_addr` (feeds traffic through the outbound pipeline) |
| `tls_bridge.mode` not `disabled`/empty | TLS bridge, so the pipeline sees decrypted HTTPS instead of an opaque CONNECT tunnel |
| `session.enabled` not `false` | session store, plus the session API on `listener.session_api_addr` when set |

A listen address of port `0` is resolved to the port the kernel actually assigned,
so an ephemeral proxy is still dialable by the hosted command.

`--sessionServer` overrides the session API address when given explicitly
(defaulting to `localhost:9094`, which leaves the config's own address alone):

```sh
# Serve the session API somewhere else, even if the config disabled sessions.
rossoctl authbridge exec --sessionServer 127.0.0.1:9500 --config ./authbridge.yaml -- claude

# Turn session tracking off entirely.
rossoctl authbridge exec --sessionServer "" --config ./authbridge.yaml -- claude
```

The command's environment is pointed at whatever started: `HTTP_PROXY` for the
forward proxy, plus `HTTPS_PROXY` and the CA trust variables
(`NODE_EXTRA_CA_CERTS`, `REQUESTS_CA_BUNDLE`, `SSL_CERT_FILE`) when the TLS
bridge runs. Variables already set in your environment are left alone. Everything
is shut down when the command exits or on SIGINT/SIGTERM.

Authbridge's own log output goes to `--logfile` (default `/tmp/authbridge.log`)
rather than stderr, so it does not interleave with the hosted command's output.
The path is printed at startup; pass `--logfile ""` to log to stderr instead.

Plugins are compiled in via one blank import per plugin, matching the authbridge
binaries — including `context-guru`, which those binaries keep opt-in but which
rossoctl links by default so the context-guru demos run without a special build.
Drop any of them with its exclude tag:

```sh
go build -tags exclude_plugin_opa,exclude_plugin_contextguru
```

Note that `go run` reports its own exit status, not the command's, so it collapses
any non-zero status to 1. Use a built binary when the exit code matters.

## Usage

```sh
rossoctl --help
rossoctl version
rossoctl agents --help

# Print instructions for installing the platform (clone + per-cluster setup script)
rossoctl install

# Manage contexts (persisted in ~/.config/rossoctl/config.yaml, kubectl-style)
# get-contexts never creates anything, so it prints an empty table until some
# other command seeds the config. Seeding creates two contexts: one for the
# default API server, which becomes current, and a local "cortex" one.
rossoctl config get-contexts
rossoctl config create-context --name dev \
    --server http://my-host:8080/api/v1/ --namespace team1 --bearer-token <token>   # becomes current
rossoctl config use-context dev
rossoctl config set-context --namespace team1   # set namespace on current context (warns if unknown to server)
rossoctl config set-context --namespace team1 --server http://other:8080/api/v1/   # also replace the server
rossoctl config set-context --name prod          # rename the current context (updates the current reference)
rossoctl login --token <token>                  # set the token on the current context directly
rossoctl login                                  # or: OAuth device flow against the server's Keycloak
rossoctl login --server http://host:8080/api/v1/ --token <token>   # target the context for that host (create if absent), make it current
# The "cortex" context is answered inside the command's own process, from the
# records `authbridge exec` wrote, so it needs no server and no token. --cortex
# makes it current without contacting anything; its namespace comes from those
# local records.
rossoctl login --cortex                         # switch to the local cortex; no network, no token
# `cortex serve` and `authbridge exec` both create that context if it is missing,
# so `login --cortex` is usually unnecessary. `cortex serve` also makes it current;
# `authbridge exec` deliberately does not, since it hosts an unrelated command and
# must not repoint later invocations. Switch back with `config use-context`.

# Inspect the bearer token stored on the context (decoded locally; nothing is sent)
rossoctl auth status                            # name/username/email, issuer, expiry, audiences, roles, scopes
rossoctl auth status --json                     # the decoded claims as JSON
rossoctl auth status --context prod             # inspect another context's token

# Print the raw bearer token, and nothing else, for use in another command.
# Note this writes a credential to stdout: a terminal keeps it in scrollback and
# CI keeps it in the build log. Exits non-zero when the context holds no token,
# so the substitution fails rather than expanding to an empty string.
rossoctl auth token
curl -H "Authorization: Bearer $(rossoctl auth token)" http://my-host:8080/api/v1/agents?namespace=team1
rossoctl auth token --context prod              # the token from another context

# Show the server's auth configuration (GET <server>/auth/config)
rossoctl auth-config
rossoctl auth-config --json
rossoctl --server http://my-host:8080/api/v1/ auth-config

# Show current session + platform status, mirroring the web UI admin page
# (GET <server>/auth/status, /auth/me, /config/platform-status)
rossoctl status
rossoctl status --json                          # raw API data as JSON

# List agents (GET <server>/agents)
rossoctl agents list                            # single namespace: agents --namespace, else current context
rossoctl agents --namespace team2 list          # list one specific namespace
rossoctl agents list --all-namespaces           # -A: discover via GET /namespaces, list across all
rossoctl agents list --all-namespaces --json    # each namespace's response, separated by ---
rossoctl agents list --no-headers               # omit the header row, for piping to other tools
# With --no-headers the "no agents found" notice goes to stderr instead, so
# stdout is empty when there is nothing to list and a pipeline sees no rows:
rossoctl agents list --no-headers | awk '{print $1}' | xargs -n1 rossoctl agents delete

# Show one agent (GET <server>/agents/<namespace>/<name>)
rossoctl agents get orders                      # single-column text, laid out like the web detail page
rossoctl agents get orders --json               # raw JSON

# Wait for an agent to become ready, polling GET <server>/agents/<namespace>/<name>
# every 2 seconds. Exits 0 as soon as it is ready, so it can gate what follows.
rossoctl agents wait orders
rossoctl agents wait orders --timeout 5m        # default 60s; --timeout 0 waits indefinitely
rossoctl agents wait orders -v                  # report progress on stderr while waiting

rossoctl agents import from-image --name orders --containerImage ghcr.io/x/y:latest \
    && rossoctl agents wait orders --timeout 5m \
    && ./run-integration-tests.sh
# Waiting ends early and non-zero when readiness will never arrive: a failed job or
# a rollout past its deadline, or a name the server does not know (404). Reporting
# that immediately beats spending the timeout to report the wrong cause.

# Show an agent's AuthBridge configuration
# (GET <server>/agents/<namespace>/<name>/identity-config): the mode, plus the
# inbound and outbound plugin pipelines in execution order, each plugin with its
# on_error policy and per-instance config.
rossoctl agents authbridge get orders
rossoctl agents authbridge get orders --json    # raw JSON

# Set an agent's AuthBridge configuration
# (PUT <server>/agents/<namespace>/<name>/identity-config). --policy-file is
# required; its bytes are sent verbatim as text/plain, so comments and key order
# survive and the server validates rather than the CLI.
rossoctl agents authbridge set orders --policy-file ./authbridge.yaml

# --wait reads the configuration before writing, then polls every 2 seconds until
# what AuthBridge reports differs from that baseline, giving up after 2 minutes.
# The comparison is against the baseline, not the file: the file is YAML written to
# a ConfigMap while the GET returns the live JSON a sidecar serves, redacted.
# Because a change is the signal, re-applying the configuration already in effect
# cannot be confirmed — it times out and exits non-zero, saying so.
rossoctl agents authbridge set orders --policy-file ./authbridge.yaml --wait

# Delete an agent (DELETE <server>/agents/<namespace>/<name>)
rossoctl agents delete orders

# Import an agent from a container image (POST <server>/agents)
rossoctl agents import from-image --name orders --containerImage ghcr.io/x/y:latest
rossoctl agents import --deployment-type sandbox from-image \
    --name orders --containerImage ghcr.io/x/y:latest --imagePullSecret regcred \
    --envVarsURL https://example.com/orders.env   # newline-separated key=value
# --envVar sets one variable inline and may be repeated. Values are taken
# literally, commas and all. Where both name the same variable, --envVar wins
# over --envVarsURL, whichever order the flags appear in.
rossoctl agents import from-image --name orders --containerImage ghcr.io/x/y:latest \
    --envVar LOG_LEVEL=debug --envVar 'TAGS=a,b,c'

# `agents --namespace` overrides the context's namespace for any agents subcommand
rossoctl agents --namespace team2 get orders    # -> GET /agents/team2/orders

# `--context` is a top-level flag: it uses a named context instead of the current
# one (its server, token, namespace). It may appear before or after the subcommand.
rossoctl --context prod agents get orders
rossoctl agents --context prod get orders               # equivalent
rossoctl --context prod agents --namespace teamX list   # --namespace still overrides the context's namespace

# Tools mirror the agents commands, against the /tools endpoint.
# --namespace, --context, --all-namespaces (-A), --json, and --no-headers behave
# as for agents.
rossoctl tools list                              # single namespace (context, or --namespace)
rossoctl tools list --all-namespaces             # discover and list across all
rossoctl tools list --no-headers | awk '{print $1}' | xargs -n1 rossoctl tools delete
rossoctl tools --namespace team2 list --json
rossoctl tools get weather-mcp                   # GET /tools/<namespace>/weather-mcp (single-column detail)
rossoctl tools get weather-mcp --json            # raw JSON response
rossoctl tools wait weather-mcp                  # poll until ready; default --timeout 60s
rossoctl tools delete weather-mcp                # DELETE /tools/<namespace>/weather-mcp
rossoctl tools import from-image --name weather-mcp --containerImage ghcr.io/x/y:latest  # POST /tools
rossoctl tools import --deployment-type statefulset from-image \
    --name weather-mcp --containerImage ghcr.io/x/y:latest --envVarsURL https://example.com/tool.env
# --envVar works as it does for agents: repeatable, literal values, and it wins over --envVarsURL
rossoctl tools import from-image --name weather-mcp --containerImage ghcr.io/x/y:latest \
    --envVar LOG_LEVEL=debug --envVar 'TAGS=a,b,c'
# --ports sets service ports as name:port:targetPort[:protocol] (default http:9090:9090:TCP); a bare "port" = http:port:port:TCP
rossoctl tools import from-image --name weather-mcp --containerImage ghcr.io/x/y:latest --ports grpc:9000:9001:TCP,8080

# A tool built from source reports "Building" until its build finishes, which is
# often longer than the 60s default, so allow for it. A failed build reports
# "Build Failed" and ends the wait immediately rather than burning the timeout on
# a workload that will never be created.
rossoctl tools wait weather-mcp --timeout 10m
# Against the local "cortex" context `tools wait` runs to its timeout: that server
# does not implement the tool detail endpoint it polls (`tools get` fails there for
# the same reason), so use `tools list` to check a local tool. `agents wait` works.

# List namespaces (GET <server>/namespaces)
rossoctl namespaces list
rossoctl namespaces list --all      # include non-rossoctl-enabled namespaces
rossoctl namespaces list --json

# Log the underlying REST requests to stderr
rossoctl -v agents list
```

## Full docs

See [the documentation](./docs)
