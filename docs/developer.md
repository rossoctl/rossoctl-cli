# Developer notes

Working notes for changing rossoctl-cli itself. See [README.md](README.md) for the
repository layout and how the command tree is organized.

## Testing against a local clone of Cortex

[Cortex](https://github.com/rossoctl/cortex) developers can use the CLI to develop Cortex
plugins without building containers.

rossoctl-cli depends on `github.com/rossoctl/cortex/authbridge/authlib` — the
authbridge plugin registry, pipeline, and listeners. By default that resolves to a
published pseudo-version, so testing a change made in Cortex would mean committing
and pushing it first.

A `replace` directive points the build at a working copy instead:

```sh
go mod edit -replace \
  github.com/rossoctl/cortex/authbridge/authlib=/path/to/cortex/authbridge/authlib
```

Note the path ends in `authbridge/authlib`, not the repository root: authlib is a
nested module with its own `go.mod`, and the replacement target has to be the
directory declaring the module path being replaced.

Confirm it took effect:

```sh
go list -m github.com/rossoctl/cortex/authbridge/authlib
# github.com/rossoctl/cortex/authbridge/authlib v0.0.0-2026... => /path/to/cortex/authbridge/authlib
```

Edits in the clone are picked up by the next `go build` or `go test`. No commit,
push, tag, or version bump is involved, and `go.sum` is not consulted for a
replaced module, so there are no checksum errors while iterating.

Restore the published version when you are done:

```sh
go mod edit -dropreplace github.com/rossoctl/cortex/authbridge/authlib
```

**Do not commit the replace!**

The directive holds an absolute path from one machine, so committing it breaks CI
and every other contributor. Two things make that easy to get wrong:

- `go mod tidy` (and so `make tidy`) rewrites `require` lines to match whatever the
  local clone imports while the replace is active, and drops authlib's `go.sum`
  lines, since a replaced module has no checksum. CI checks that `go mod tidy`
  produces no diff, so a tidy run against a local clone can fail CI even after the
  replace itself is dropped. Drop the replace *before* running tidy.
- `go.mod` is a file you edit for many other reasons, so the stray line is easy to
  sweep into an unrelated commit.

`git diff go.mod` before committing is enough to catch both.

### Alternative: a go.work file, which cannot be committed by accident

A workspace has the same effect, in a file that is gitignored rather than tracked:

```sh
go work init . /path/to/cortex/authbridge/authlib
printf 'go.work\ngo.work.sum\n' >> .gitignore   # if not already ignored
```

`go.work` takes precedence over `go.mod`, so no `replace` is needed and `go.mod`
is never touched. Remove it with `rm go.work go.work.sum` (or set
`GOWORK=off` for a single command) to build against the published version again.

Prefer this when the Cortex work will take more than an afternoon; prefer
`go mod edit` for a quick check you will undo in the same sitting.

### Verifying you are really building the local copy

A replace that silently did not apply looks exactly like one that did. The
unambiguous check is to break the local copy on purpose:

```sh
echo 'func probe() { not valid go }' >> /path/to/cortex/authbridge/authlib/plugins/registry.go
go build ./...     # must fail, naming the path in the clone
git -C /path/to/cortex checkout authbridge/authlib/plugins/registry.go
```

If the build succeeds, the replace is not in effect and you are compiling the
published module.

## Running the tests

```sh
make test                      # go test ./...
make vet                       # go vet ./...
gofmt -l .                     # lists files needing formatting; make fmt rewrites them
make tidy                      # go mod tidy
```

The full suite takes roughly ten seconds. It needs no services, credentials, or
network access: tests bind ephemeral localhost ports, point `HOME` at a temp
directory, and the container tests assert on the command strings they *would* run
rather than invoking a real runtime.

While iterating, narrow the run:

```sh
go test ./internal/otelcollect/                    # one package
go test ./cmd/ -run TestAgentsImport -v            # tests matching a pattern, verbosely
go test ./cmd/ -run 'TestExecWithClaudeOtel' -count=1
```

`-run` takes a regular expression matched against the test name, and it also
selects subtests when given a `Parent/Sub` path.

### What CI runs

`.github/workflows/ci.yml` runs the checks above plus three separate test passes.
Reproduce all of them before pushing:

```sh
gofmt -l .                        # must print nothing
go mod tidy && git diff --exit-code -- go.mod go.sum
make vet
go build ./...
go test ./... -count=1
go test ./... -race -count=1
go test ./... -count=1 -shuffle=on
```

Why each pass exists, rather than one combined run:

- **`-count=1`** defeats the test cache, so a green result cannot stand in for a
  run that never happened on this commit. Worth using locally too, especially after
  changing anything outside the package under test.
- **`-race`** runs separately from the plain pass so an ordinary failure is not
  reported as a data race.
- **`-shuffle=on`** matters more here than in most repos. The suite mutates process
  state — `HOME`, `XDG_CONFIG_HOME`, cobra flag values — and `cmd/root_test.go`
  documents a pflag hazard where a slice flag's first `Set` in a later test appends
  instead of replacing. Shuffling surfaces an order dependency in CI rather than on
  someone's unrelated pull request.

A test that passes alone and fails under `-shuffle=on` is almost always leaked
process state. `isolateHome(t)` (see `cmd/config_test.go`) is how the existing tests
avoid touching the real `~/.config/rossoctl`.

### Tests that shell out

Some tests run a real child process (`authbridge exec` hosts a command) or a
stubbed container runtime. They are still hermetic — the stub is a shell script
written into `t.TempDir()` and selected with `ROSSOCORTEX_RUNTIME` — but they do
depend on `sh`, `env`, and `printf` being on `PATH`, so they are not expected to
pass on a machine without a POSIX shell.

`go run` reports its own exit status rather than the child's, collapsing any
non-zero status to 1. Use `make build` and run `./bin/rossoctl` when an exit code
matters.
