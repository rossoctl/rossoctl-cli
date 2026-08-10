## Installation tip

By default _downloadRossoctl_ installs the latest release; pin a version with
`ROSSOCTL_CLI_VERSION`:

```sh
curl -fsSL https://raw.githubusercontent.com/kagenti/rossoctl-cli/main/downloadRossoctl \
  | ROSSOCTL_CLI_VERSION=v0.1.0 sh
```

The script prints how to add `$HOME/.config/rossoctl` to your `PATH`. Each release
ships prebuilt binaries built by `.github/workflows/release.yml`; the asset
names are `rossoctl-<version>-<uname>-<uname -m>.tar.gz` (arm64 is labeled
`arm64` on both Linux and Darwin).

## Layout

This project follows the standard Go CLI layout:

```
.
├── main.go                     # Thin entry point; calls cmd.Execute()
├── cmd/                        # Cobra command tree (grouped by command)
│   ├── root.go                 # Root command + Execute() + persistent flags
│   ├── version.go              # `rossoctl version`
│   ├── unimplemented.go        # newGroup/newLeaf helpers + UNIMPLEMENTED stub
│   ├── install.go              # `rossoctl install` (prints setup instructions)
│   ├── status.go               # `rossoctl status` (session + platform status)
│   ├── login.go                # `rossoctl login` (--token or OAuth device flow)
│   ├── auth_status.go          # `rossoctl auth status` (decodes the stored token's claims)
│   ├── agents.go               # `rossoctl agents ...` (`list` fetches GET /agents)
│   ├── authconfig.go           # `rossoctl auth-config` (shows server auth config)
│   ├── config.go               # `rossoctl config ...` (context management)
│   ├── namespaces.go           # `rossoctl namespaces ...` (`list` fetches GET /namespaces)
│   ├── tools.go                # `rossoctl tools ...` (list/get/delete/import, mirrors agents)
│   └── ui.go                   # `rossoctl ui open` (opens the context server's site root)
├── internal/                   # Private application logic (not importable externally)
│   ├── apiclient/              # HTTP client for the Rossoctl backend API
│   ├── buildinfo/              # Version metadata formatting
│   ├── config/                 # ~/.config/rossoctl/config.yaml context persistence
│   ├── deviceflow/             # OAuth 2.0 device authorization grant (Keycloak)
│   └── jwt/                    # Unverified JWT claim decoding (for inspection only)
├── Makefile
└── go.mod
```

Design principles:

- **`main.go` stays trivial** — it only calls `cmd.Execute()`.
- **`cmd/` handles the CLI surface** — flag parsing, help text, and wiring.
  Each command lives in its own file and registers itself with `rootCmd` in
  `init()`.
- **`internal/` holds the real logic** — packages there are free of Cobra and
  of I/O, so they can be unit-tested directly. `internal/` also prevents other
  modules from importing this code.

## Build from source

```sh
make build      # -> ./bin/rossoctl (version info injected via -ldflags)
make install    # install into $GOBIN
make test       # go test ./...
```

### Contexts and server resolution

Contexts are persisted in `~/.config/rossoctl/config.yaml` (directory `0700`, file
`0600`). Each context has a name, a server URI, an optional namespace, and an
optional bearer token.
The file is created lazily — the first command that needs it seeds a context
from the default server (`http://kagenti-ui.localtest.me:8080/api/v1/`) and
makes it current. Creating a context makes it current.

The server a command talks to is resolved as: an explicit `--server` flag wins
(and no bearer token is sent); otherwise the current context supplies both the
server URI and its bearer token. The global `--server` and `--verbose`/`-v`
flags must appear before the subcommand; `-v` logs each REST request (method,
URL, status, timing) to stderr.

### Logging in

`rossoctl login --token <token>` stores a token on a context. With `--server`,
the token is stored on the context named after that server's hostname (created
if none exists), which becomes current; without `--server`, it is stored on the
current context. `rossoctl login` (no `--token`) runs the OAuth 2.0 device
authorization grant (RFC 8628): it reads `keycloak_url`, `realm`, and
`client_id` from `GET <server>/auth/config`, requests a device code from
Keycloak, prints a verification URL and one-time code (and best-effort opens a
browser), polls until you authorize, and saves the resulting bearer token on
the target context. It finishes by pointing at `rossoctl auth status`, since the
token's roles and audiences decide which operations will now succeed.

### Inspecting the stored token

`rossoctl auth status` decodes the bearer token on the effective context and
prints its claims: name, preferred username, email, subject, issuer, expiration
(with a leading `WARNING` line once it has passed), audiences,
`realm_access.roles`, and scopes. `--json` prints the decoded claims instead.

Nothing is sent to the server — the token is read from the config file and
decoded locally, so it works against a server that is down or that is rejecting
the token. The signature is deliberately **not** verified: this reports what the
token asserts about itself, which is a diagnostic, never an authorization
decision. Contrast the two neighbouring commands: `rossoctl status` is the
server's view of the session, and `rossoctl auth-config` is the server's
authentication settings.

Only JWTs can be read this way. `rossoctl login --token` accepts any string, and
an OAuth server may issue an opaque token; for one of those the command fails and
names `rossoctl status` as the way to ask the server instead.

The command tree mirrors the subcommands referenced in the Rossoctl docs
(`agents`, `config`, `namespaces`, `tools`, `ui`,
plus `auth-config` and the top-level `install`, `login`, `status`). The
`config` context commands, `login`, `auth status`, `auth-config`, `install`, `status`,
`agents list`, `agents get`, `agents delete`, `agents import from-image`,
`tools list`, `tools get`, `tools delete`, `tools import from-image`,
`namespaces list`, and `ui open` are implemented; other leaf commands currently
print `UNIMPLEMENTED` as a placeholder.
