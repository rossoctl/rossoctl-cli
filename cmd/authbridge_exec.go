package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
	"github.com/rossoctl/cortex/authbridge/authlib/listener/forwardproxy"
	"github.com/rossoctl/cortex/authbridge/authlib/listener/reverseproxy"
	"github.com/rossoctl/cortex/authbridge/authlib/listener/skiphost"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/sessionapi"
	"github.com/rossoctl/cortex/authbridge/authlib/shared"
	"github.com/rossoctl/cortex/authbridge/authlib/spiffe"
	"github.com/rossoctl/cortex/authbridge/authlib/tlsbridge"

	"github.com/rossoctl/rossoctl-cli/internal/instances"
)

// execArgs holds the `authbridge exec` flags.
var execArgs struct {
	config              string
	logfile             string
	sessionServer       string
	proxyContainerImage string
	instanceName        string
	namespace           string
	inboundProtocol     string
}

// defaultSessionServer is where the session API listens when --sessionServer is
// not given. It documents the usual address rather than forcing it: the flag only
// overrides the config when explicitly set.
const defaultSessionServer = "localhost:9094"

// defaultLogfile is where authbridge's own log output goes when --logfile is not
// given. It is a file rather than stderr so the hosted command's output stays
// readable: the pipeline, proxy, and plugins all log through slog, which would
// otherwise interleave with the command's own stderr.
const defaultLogfile = "/tmp/authbridge.log"

const (
	// maxConfigBytes caps how much of a remote --config document is read, so a
	// wrong URL pointing at something enormous fails fast instead of exhausting
	// memory.
	maxConfigBytes = 1 << 20 // 1 MiB

	// configFetchTimeout bounds the HTTP GET for a remote --config.
	configFetchTimeout = 30 * time.Second

	// pipelineStartTimeout bounds pipeline startup, mirroring the authbridge
	// binaries' 60s initialization budget.
	pipelineStartTimeout = 60 * time.Second

	// shutdownTimeout bounds graceful shutdown of the session API and the
	// outbound pipeline, mirroring the authbridge binaries' 15s budget.
	shutdownTimeout = 15 * time.Second
)

// Session store defaults, used when the config's session block leaves them
// unset. These mirror the authbridge binaries' own fallbacks.
const (
	defaultSessionTTL         = 30 * time.Minute
	defaultSessionMaxEvents   = 500
	defaultSessionMaxSessions = 100
)

// tlsBridgeModeDisabled is the tls_bridge mode that turns the bridge off. An
// empty mode means the same thing.
const tlsBridgeModeDisabled = "disabled"

// caCertFileName is the client trust anchor EnsureFileSource writes into the
// bridge's ca_dir. It is what the CA trust variables point the child at.
const caCertFileName = "ca.crt"

// caTrustVars are the environment variables that point a child's TLS stack at
// the bridge's CA. One per ecosystem, because there is no single standard:
// Node reads NODE_EXTRA_CA_CERTS, Python-requests reads REQUESTS_CA_BUNDLE,
// and OpenSSL/Go read SSL_CERT_FILE.
var caTrustVars = []string{
	"NODE_EXTRA_CA_CERTS",
	"REQUESTS_CA_BUNDLE",
	"SSL_CERT_FILE",
}

// spiffeIdentityType is the `identity.type` config value selecting the SPIFFE
// identity scheme. Mirrors the constant in the authbridge binaries, which keep
// it local so main stays decoupled from any specific plugin package.
const spiffeIdentityType = "spiffe"

// exitCodeError carries a child process's exit status out of RunE so Execute
// can exit with that same code. It is not an ordinary failure: the command ran
// and reported a status, so Execute exits silently with that status rather than
// printing an "Error:" line.
type exitCodeError struct {
	code int
}

func (e *exitCodeError) Error() string {
	return fmt.Sprintf("command exited with status %d", e.code)
}

var authbridgeExecCmd = &cobra.Command{
	Use:   "exec --config FILE|URL -- COMMAND [ARG...]",
	Short: "Run a command with an authbridge pipeline",
	Long: `Run a command hosted by an authbridge pipeline.

--config is required and names an authbridge YAML config, either a local file
path or an http/https URL. A remote config is fetched into a temporary file
(config loading is path-based) and that file is removed before exec returns.

Everything after the "--" end-of-options delimiter is passed through to the
command unchanged: flags in it are the command's own, never rossoctl's. The
command is run with its stdin, stdout, and stderr connected to rossoctl's, and
rossoctl exits with the command's exit status.

While the command runs, exec hosts it behind the config's plugin pipelines. Both
the inbound and outbound pipelines are built and started; listener.roles decides
which listeners feed traffic into them:

  - The forward proxy listens on listener.forward_proxy_addr when
    listener.roles includes "forward". This is what feeds the command's own
    egress through the outbound pipeline, so without it that pipeline is built
    but never invoked.
  - The reverse proxy listens on listener.reverse_proxy_addr when
    listener.roles includes "reverse", and forwards to
    listener.reverse_proxy_backend — usually the command itself. Callers reach
    the command through this address, and their requests run the inbound
    pipeline on the way in. Its bound address is printed at startup, so a
    port of 0 still tells you where to connect.
  - The TLS bridge terminates the command's outbound TLS when tls_bridge is
    present and not disabled, so the pipeline sees decrypted HTTPS instead of an
    opaque CONNECT tunnel.
  - The session store records traffic unless session.enabled is false, and the
    session API serves it on listener.session_api_addr when that is set.
    --sessionServer overrides that address when given, and --sessionServer ""
    turns session tracking off entirely.

mTLS is NOT enforced. exec runs without a SPIFFE identity, because obtaining one
blocks on the SPIRE workload API, so the listeners accept plaintext only. An
mtls.mode of permissive runs with a notice; strict is refused rather than
silently downgraded, since serving plaintext to every caller is the opposite of
what it asks for. Run the pipeline in a cluster to enforce it.

The command's environment points it at what was started: HTTP_PROXY when the
forward proxy runs, and additionally HTTPS_PROXY plus the CA trust variables
(NODE_EXTRA_CA_CERTS, REQUESTS_CA_BUNDLE, SSL_CERT_FILE) when the TLS bridge
runs. A variable already set in rossoctl's own environment is left alone. The
reverse proxy adds nothing: it is where callers reach the command, not where the
command sends its own traffic.

With --proxyContainerImage the pipeline runs in a container from that image
instead of in this process. The config is mounted read-only at /tmp/config.yaml
and passed as --config; ports 8000, 8081, 9093, and 9094 are published on
ephemeral host ports, discovered after startup, and the proxy variables point at
the host port for 8081. When the config's tls_bridge generates its own CA, a
temporary directory is mounted at tls_bridge.ca_dir to receive it, exec waits up
to 30s for ca.crt to appear there, and the CA trust variables point at it. The
container is stopped and the temporary directory removed when the command exits.
This needs docker or podman on PATH ($ROSSOCORTEX_RUNTIME overrides which).

While the command runs, exec records the instance as a <name>.json file in
~/.config/rossocortex/namespaces/<namespace>, and removes it when the command
exits. The file names the instance, its namespace, the proxy container (with
--proxyContainerImage), the inbound, session, and admin addresses as reached from
this host, the inbound protocol, and the command line — so another tool can find
a running instance and where to reach it.

--instanceName sets the recorded name instead of generating one. Since the name
is the file name, it must be unique within the namespace: a name already in use
is refused rather than overwriting the running instance that holds it.
--namespace chooses the namespace, defaulting to the current context's and
falling back to "team1" when the context has none. Both must be usable as a
single path component — letters, digits, '-', '_' and '.'. --inboundProtocol
records whether the inbound listener fronts a2a (the default) or mcp.

The record is advisory: a process killed with SIGKILL cannot remove its file, so
a reader should treat one as a claim to verify rather than proof. A stale file
also keeps its name claimed, so restarting with the same --instanceName after a
SIGKILL means deleting the leftover record first.

Authbridge's own log output goes to --logfile (default /tmp/authbridge.log), not
to stderr, so it does not interleave with the command's output. The path is
printed at startup. Pass --logfile "" to log to stderr instead.

Everything is shut down gracefully when the command exits or when rossoctl
receives SIGINT or SIGTERM.

The session API is UNAUTHENTICATED and may expose raw prompts, completions, and
tool results. Bind it to a loopback address only.`,
	Example: `  # Run claude behind the pipeline in a local config.
  rossoctl authbridge exec --config ./authbridge.yaml -- claude "explain this repo"

  # Take the config from a URL; it is fetched to a temp file and removed on exit.
  rossoctl authbridge exec --config https://example.com/authbridge.yaml -- ./script.sh --verbose`,
	// The command line after "--" is the child's, so it may hold any number of
	// arguments; cobra hands them to us verbatim.
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runCortexExec(cmd, args)
	},
}

// runCortexExec validates the invocation — a command after "--" and a --config
// — and sets up logging, then hands off to execWithPipeline, whose error (the
// command's exit status, or a setup failure) it returns unchanged.
//
// It deliberately does not resolve a context. exec is configured entirely by
// --config, so it needs nothing from the context config: no server, token, or
// namespace is ever read. It used to call resolveCortexContext, which creates a
// cortex-typed context and makes it *current* as a side effect — meaning running
// a command behind a pipeline silently repointed every later rossoctl
// invocation at a different context. Nothing here consumed the result, so the
// call was pure side effect and is gone.
func runCortexExec(cmd *cobra.Command, args []string) error {
	argv, err := passthroughArgs(cmd, args)
	if err != nil {
		return err
	}

	// --config is required: exec's whole purpose is to host a command behind a
	// configured pipeline, and there is no meaningful default pipeline.
	if execArgs.config == "" {
		return fmt.Errorf("--config is required (a local YAML file path or an http/https URL)")
	}

	// exec ignores the context, but an explicit --context naming one that does
	// not exist is still reported rather than silently accepted: it is almost
	// certainly a typo, and staying quiet would hide it. The lookup is read-only
	// (loadConfigReadOnly, not loadConfig), so a missing config file stays
	// missing and no context is created.
	if contextOverride != "" {
		cfg, err := loadConfigReadOnly()
		if err != nil {
			return err
		}
		if _, ok := cfg.Get(contextOverride); !ok {
			return fmt.Errorf("no context named %q", contextOverride)
		}
	}

	// Redirect authbridge's logging to the logfile before anything that logs
	// runs, and tell the operator where it went — otherwise the pipeline's own
	// output would vanish silently.
	logClose, err := setupLogging(cmd)
	if err != nil {
		return err
	}
	defer logClose()

	return execWithPipeline(cmd, argv)
}

// authbridgeHost is what a proxy implementation hands back to execWithPipeline:
// the environment the child needs to reach it, the channel an async listener
// failure arrives on, and where its listeners ended up.
//
// stop tears the host down. It is the counterpart of the constructor's stepwise
// setup, so it must be called on every exit path — including a setup failure
// partway through, where it undoes only what was built.
type authbridgeHost struct {
	// env is the child's environment, already carrying the proxy and CA trust
	// variables. Nil means "inherit rossoctl's" (see exec.Cmd.Env).
	env []string

	// serveErr carries an async listener failure from its goroutine back to the
	// command loop. Never nil.
	serveErr <-chan error

	// stop shuts the host down in reverse construction order. Never nil.
	stop func()

	// addrs is where this host's listeners can be reached from this machine, for
	// the instance record. It is reported by the constructor rather than derived
	// from the config by the caller, because the two paths arrive at these
	// addresses differently: the in-process path knows the addresses it bound,
	// while the container path has to ask the runtime which host ports it
	// published.
	addrs hostAddrs
}

// hostAddrs are an authbridgeHost's listener addresses as reached from this
// machine, each empty when there is no such listener.
//
// "As reached from this machine" is the point: for a container these are the
// published host ports, not the ports bound inside it. Recording the inside
// address would name something unreachable.
type hostAddrs struct {
	// containerName is the proxy container's name, empty when the pipeline runs
	// in this process.
	containerName string

	// inbound is the reverse proxy — where callers reach the hosted service.
	inbound string

	// session is the session events endpoint (the 9094 listener).
	session string

	// admin is the stats/config endpoint (the 9093 listener). The in-process
	// path leaves this empty: it starts no admin server, so there is no address
	// to report. Only the container path has one, because the image serves it.
	admin string
}

// execWithPipeline loads the config, brings the host up around the child
// command, runs the command, and tears the host down once it exits or a signal
// arrives.
//
// The returned error is the child's status (as an *exitCodeError) when the
// command ran, or a setup failure otherwise.
func execWithPipeline(cmd *cobra.Command, argv []string) error {
	// Materialize the config as a local file: config.Load is path-based, so a
	// remote config must be written to disk first. cleanup removes the temp file
	// (and only a temp file — never a user's own config).
	path, cleanup, err := materializeConfig(cmd, execArgs.config)
	if err != nil {
		return err
	}
	defer cleanup()

	cfg, err := config.Load(path)
	if err != nil {
		return fmt.Errorf("loading config %s: %w", execArgs.config, err)
	}

	// All three are resolved before anything starts, so a bad --inboundProtocol,
	// an unusable --namespace, or an --instanceName already in use is reported
	// without having brought a pipeline up and torn it down again.
	//
	// The name in particular has to be known this early: the container is named
	// after the instance, so the name cannot be left for instances.Create to
	// generate after the fact. The namespace has to precede the name because the
	// name's uniqueness check is scoped to it.
	proto, err := instances.ParseProtocol(execArgs.inboundProtocol)
	if err != nil {
		return err
	}
	namespace, err := execNamespace()
	if err != nil {
		return err
	}
	name, err := instanceName(namespace)
	if err != nil {
		return err
	}

	// --proxyContainerImage hosts the pipeline in a container instead of in this
	// process. The two paths produce the same *authbridgeHost, so everything from
	// here down — the child command, its environment, the teardown — is common.
	host, err := startHost(cmd, cfg, path, name)
	if err != nil {
		return err
	}
	defer host.stop()

	// Record the instance for as long as the child runs. This is deliberately
	// after startHost: the record names the addresses the listeners actually
	// bound, which are not known until they are up.
	//
	// Registered *before* runPassthrough and removed after, so the file exists
	// exactly while the command is being hosted. The removal is deferred rather
	// than done at the end of the function body, so a signal-driven return path
	// cleans up the same as a normal exit.
	rec, err := registerInstance(cmd, argv, name, namespace, proto, host.addrs)
	if err != nil {
		return err
	}
	defer unregisterInstance(cmd, rec)

	return runPassthrough(cmd, argv, host.env, host.serveErr)
}

// instanceName returns the name to record for this run: --instanceName when
// given, and a generated one otherwise.
//
// A supplied name is checked for a clash here, before anything is started, so a
// duplicate is reported without having brought a pipeline up and torn it down
// again. The check is advisory — two exec invocations racing on the same name
// could both pass it — and instances.Create makes the same check atomically when
// it writes. This one exists to fail early and with a clearer message.
//
// A generated name is not checked: NewName's random suffix makes a clash
// unlikely, and Create would report one anyway.
func instanceName(namespace string) (string, error) {
	if execArgs.instanceName == "" {
		return instances.NewName()
	}

	name := execArgs.instanceName
	if err := instances.ValidName(name); err != nil {
		return "", fmt.Errorf("--instanceName %q is not usable as a file name: %w", name, err)
	}
	taken, err := instances.Exists(namespace, name)
	if err != nil {
		return "", err
	}
	if taken {
		return "", fmt.Errorf("an instance named %q already exists in namespace %q; "+
			"pick another --instanceName or stop the instance using it", name, namespace)
	}
	return name, nil
}

// execNamespace returns the namespace to record this instance in: --namespace
// when given, otherwise the current context's namespace, falling back to
// instances.DefaultNamespace.
//
// The fallback is why this does not use currentNamespace: that reports an unset
// namespace as an error, which is right for a command about to query a server but
// wrong here. exec hosts a local process that has to be recorded somewhere, and
// refusing to run because a context lacks a namespace would block a workflow that
// never needed one.
//
// A context that cannot be read is likewise not fatal — exec is configured
// entirely by --config and does not otherwise need a context, so a missing or
// unreadable config file falls back rather than failing. An explicit --context
// naming a nonexistent context is still rejected, but that check happens earlier
// in RunE.
//
// The lookup is read-only, and deliberately not resolveContext: that seeds a
// default context on first use, and exec must not create a config file as a side
// effect of recording an instance.
func execNamespace() (string, error) {
	if ns := execArgs.namespace; ns != "" {
		if err := instances.ValidName(ns); err != nil {
			return "", fmt.Errorf("--namespace %q is not usable as a directory name: %w", ns, err)
		}
		return ns, nil
	}

	if ns := contextNamespaceOrEmpty(); ns != "" {
		// A context namespace still has to be usable as a directory name; one
		// that is not is reported rather than silently replaced, since the
		// operator would otherwise not know where the record went.
		if err := instances.ValidName(ns); err != nil {
			return "", fmt.Errorf("the current context's namespace %q is not usable as a directory name: %w",
				ns, err)
		}
		return ns, nil
	}
	return instances.DefaultNamespace, nil
}

// contextNamespaceOrEmpty returns the effective context's namespace without
// creating a context, or "" when there is none to read.
//
// Every failure yields "" rather than an error: the only caller has a working
// fallback, and exec does not otherwise depend on a context, so a missing config
// file is an ordinary state here rather than something to report. Note that this
// package's `config` identifier is authbridge's config package, which is why the
// namespace is returned as a string rather than a *config.Context.
func contextNamespaceOrEmpty() string {
	cfg, err := loadConfigReadOnly()
	if err != nil {
		return ""
	}
	if contextOverride != "" {
		// RunE already rejected an unknown --context; treat it as absent.
		if ctx, ok := cfg.Get(contextOverride); ok {
			return ctx.Namespace
		}
		return ""
	}
	if ctx, ok := cfg.Current(); ok {
		return ctx.Namespace
	}
	return ""
}

// registerInstance writes the instance file describing this run.
func registerInstance(
	cmd *cobra.Command,
	argv []string,
	name, namespace string,
	proto instances.Protocol,
	addrs hostAddrs,
) (*instances.Handle, error) {
	rec, err := instances.Create(instances.Instance{
		Name:            name,
		Namespace:       namespace,
		ContainerName:   addrs.containerName,
		InboundAddr:     addrs.inbound,
		InboundProtocol: proto,
		SessionAddr:     addrs.session,
		AdminAddr:       addrs.admin,
		CommandLine:     argv,
	})
	if err != nil {
		// Fatal rather than a warning: the file is what makes a running instance
		// discoverable, and silently starting an instance nothing can find would
		// be worse than not starting it. A failure here means either the config
		// directory is unwritable or the name was claimed since it was checked —
		// both things the operator should see rather than have papered over.
		return nil, err
	}

	if verbose {
		fmt.Fprintf(cmd.ErrOrStderr(), "registered instance %s in namespace %s at %s\n",
			rec.Instance.Name, rec.Instance.Namespace, rec.Path)
	}
	return rec, nil
}

// unregisterInstance removes the instance file, reporting a failure without
// changing the command's exit status.
//
// A removal failure is only a warning: by the time it runs the child has already
// exited, and its status is what the operator asked about. A stale file is a
// nuisance for whoever reads the directory next, not a reason to report this run
// as failed — which is also why the record is documented as advisory.
func unregisterInstance(cmd *cobra.Command, rec *instances.Handle) {
	if err := rec.Remove(); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n", err)
	}
}

// startHost brings up the pipeline the child command runs behind, in a container
// when --proxyContainerImage names one and in this process otherwise.
//
// cfgPath is the realized config file, which only the container path needs: it
// mounts the file rather than reusing the already-parsed cfg.
//
// instName is this run's instance name, likewise needed only by the container
// path, which names the container after it.
func startHost(cmd *cobra.Command, cfg *config.Config, cfgPath, instName string) (*authbridgeHost, error) {
	if execArgs.proxyContainerImage != "" {
		// ApplyPreset and Validate would otherwise be skipped on this path: the
		// container does its own loading, but the CA-directory decision below
		// reads fields a preset fills in, and a config broken enough to fail
		// validation is better reported here than as a container that exits.
		config.ApplyPreset(cfg)
		if err := config.Validate(cfg); err != nil {
			return nil, fmt.Errorf("invalid config: %w", err)
		}
		return startAuthbridgeContainer(cmd, cfg, cfgPath, execArgs.proxyContainerImage,
			instances.NewContainerName(instName))
	}
	return startAuthbridgeHost(cmd, cfg)
}

// startAuthbridgeHost builds and starts everything the child command runs
// behind — both plugin pipelines, the session and shared stores, the TLS bridge,
// the forward and reverse proxies, and the session API — and returns the child's
// environment plus the teardown for all of it.
//
// Teardown is accumulated as the host is built rather than deferred, because it
// has to outlive this function: the caller runs the command and only then tears
// the host down. On a setup failure the accumulated teardown runs here, so a
// half-built host never escapes.
func startAuthbridgeHost(cmd *cobra.Command, cfg *config.Config) (*authbridgeHost, error) {
	errOut := cmd.ErrOrStderr()

	// stops holds the teardown steps in construction order; stop runs them in
	// reverse, the same order deferred calls would have.
	var stops []func()
	stop := func() {
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}
	// fail tears down what was built and reports why setup stopped.
	fail := func(err error) (*authbridgeHost, error) {
		stop()
		return nil, err
	}

	// Apply the mode's preset and validate before building anything, so a bad
	// config is reported as such rather than as an obscure plugin error. This
	// mirrors the authbridge binaries' boot sequence.
	config.ApplyPreset(cfg)

	// --sessionServer overrides the config, so it is applied after the preset:
	// ApplyPreset substitutes ":9094" for an empty address, which would undo an
	// explicit request to turn the session API off.
	applySessionServerOverride(cmd, cfg, errOut)

	if err := config.Validate(cfg); err != nil {
		return fail(fmt.Errorf("invalid config: %w", err))
	}

	// The SPIFFE provider is built only when something actually consumes it:
	// NewProvider blocks until the SPIRE Workload API returns the first SVID, so
	// constructing it on the mere presence of a `spiffe:` block would hang on a
	// host without SPIRE. Need-driven construction matches authbridge-proxy.
	var provider *spiffe.Provider
	if cfg.SPIFFE != nil && spiffeProviderNeeded(cfg) {
		slog.Warn("SPIFFE configuration ignored for local testing")
	}

	// With no provider there is no X509Source, so no listener here can present a
	// certificate or verify a peer's. Strict mode means "reject callers that do
	// not present one" — honoring it is impossible, and proceeding would serve
	// plaintext to everyone, the exact opposite of what was asked for. Refusing
	// to start is the only honest option.
	//
	// Permissive is different: a permissive listener accepts plaintext alongside
	// TLS, so a plaintext-only listener is a subset of what was requested rather
	// than a contradiction of it.
	if cfg.MTLS != nil {
		if cfg.MTLS.ResolvedMode() == config.MTLSModeStrict {
			return fail(fmt.Errorf("mtls.mode: strict requires a SPIFFE workload API, " +
				"which `authbridge exec` does not connect to; use permissive for local " +
				"testing, or run the pipeline in a cluster"))
		}
		// Unconditional, not verbose-gated: an operator who wrote an mtls block
		// has a security expectation, and quietly not meeting it should be loud.
		fmt.Fprintln(errOut, "mtls is configured but not enforced: `authbridge exec` runs "+
			"without a SPIFFE identity, so listeners accept plaintext only")
	}

	// A direction with no plugins passes everything through. That is legal and
	// occasionally intended, but it is worth saying next to the config rather
	// than leaving it to be inferred from the first request.
	config.WarnEmptyPipelines(cfg, slog.Default())

	// Both directions are built regardless of which roles are active, mirroring
	// authbridge-proxy: roles select which *listeners* open, not which pipelines
	// exist. The session API reports both compositions, so an inbound pipeline no
	// listener drives is still visible to an operator inspecting the host.
	//
	// Building it is also not optional for the reverse proxy: pipeline.NewHolder
	// requires a non-nil pipeline, and reverseproxy.NewServer's director calls
	// through the holder on every request, so a nil pipeline would panic on the
	// first one. An empty plugin list yields a valid pass-through pipeline.
	inbound, err := plugins.BuildWithSPIFFE(cfg.Pipeline.Inbound.Plugins, provider)
	if err != nil {
		return fail(fmt.Errorf("building inbound pipeline: %w", err))
	}

	outbound, err := plugins.BuildWithSPIFFE(cfg.Pipeline.Outbound.Plugins, provider)
	if err != nil {
		return fail(fmt.Errorf("building outbound pipeline: %w", err))
	}

	startCtx, cancelStart := context.WithTimeout(cmd.Context(), pipelineStartTimeout)
	defer cancelStart()
	if err := outbound.Start(startCtx); err != nil {
		return fail(fmt.Errorf("starting outbound pipeline: %w", err))
	}
	stops = append(stops, func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		outbound.Stop(stopCtx)
	})

	// Inbound shares outbound's start budget, as the authbridge binaries' single
	// initialization context does. Its teardown is registered after outbound's, so
	// the reverse-order stop takes inbound down first and an in-flight inbound
	// request can still complete its outbound leg.
	if err := inbound.Start(startCtx); err != nil {
		return fail(fmt.Errorf("starting inbound pipeline: %w", err))
	}
	stops = append(stops, func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		inbound.Stop(stopCtx)
	})

	// Warm the plugin catalog so a factory violating the constructor contract
	// surfaces here rather than on the first /v1/plugins request.
	plugins.WarmCatalog()

	inboundH := pipeline.NewHolder(inbound)
	outboundH := pipeline.NewHolder(outbound)

	// Inbound plugins with no reverse listener to drive them were built and
	// started but will never be called. Say so rather than letting an operator
	// conclude their jwt-validation is protecting anything. Stderr and
	// unconditional: this is a "your config does not do what you think" message,
	// and slog goes to --logfile where it would not be seen.
	if len(cfg.Pipeline.Inbound.Plugins) > 0 && !cfg.Listener.ActiveRoles()[config.RoleReverse] {
		fmt.Fprintln(errOut, "pipeline.inbound has plugins but listener.roles does not "+
			"include \"reverse\"; inbound plugins will never run")
	}

	// The session store backs both the forward proxy's recording and the session
	// API's reads, so it is created once, before either. Session tracking
	// defaults to on; an explicit `session.enabled: false` opts out.
	var sessions *session.Store
	if cfg.Session.SessionEnabled() {
		ttl, maxEvents, maxSessions := sessionBounds(cfg, errOut)
		sessions = session.New(ttl, maxEvents, maxSessions)
		stops = append(stops, sessions.Close)
		if verbose {
			fmt.Fprintf(errOut, "session tracking enabled (ttl %s, maxEvents %d, maxSessions %d)\n",
				ttl, maxEvents, maxSessions)
		}
	}

	// The shared store is process-scoped state both proxies read and write: a
	// credential placeholder written by an inbound plugin is resolved by an
	// outbound one, which only works if the two listeners hold the same store.
	// Created unconditionally — it is cheap, and a nil store would silently
	// disable placeholder resolution rather than fail visibly.
	//
	// New starts a TTL janitor goroutine, so Close has to run on every exit path.
	// It is registered here rather than deferred because teardown outlives this
	// function; Close is safe to call more than once. Registering it before the
	// proxies means the reverse-order stop closes it after both have stopped
	// serving, so no in-flight request sees a store whose janitor has gone. That
	// ordering matters here in a way it does not in the authbridge binaries,
	// where the equivalent defer runs as the process exits.
	sharedStore := shared.New()
	stops = append(stops, sharedStore.Close)

	// The TLS bridge must exist before the forward proxy, which consumes it.
	// A nil engine leaves the proxy's blind-CONNECT-tunnel behavior intact.
	bridge, caCertPath, err := buildTLSBridge(cfg, errOut)
	if err != nil {
		return fail(err)
	}

	// serveErr carries an async listener failure from its goroutine back to the
	// command loop. Only the session API writes to it: both proxies report a bind
	// failure synchronously, and a later serve failure only reaches slog, so a
	// mid-run death there shows up in --logfile rather than here. Buffered so the
	// writing goroutine never blocks even when nobody is selecting.
	serveErr := make(chan error, 2)

	// Start the forward proxy when the forward role is active. This is what
	// actually feeds traffic through the outbound pipeline: without it the
	// pipeline is built but never invoked.
	proxyAddr, proxySrv, err := startForwardProxy(cfg, outboundH, sessions, bridge, sharedStore, errOut)
	if err != nil {
		return fail(err)
	}
	stops = append(stops, func() { shutdownHTTP(proxySrv, "forward proxy", errOut) })

	// The reverse proxy is registered after the forward one so it shuts down
	// first: stop accepting new inbound work before tearing down the egress path
	// that work depends on.
	reverseSrv, inboundAddr, err := startReverseProxy(cfg, inboundH, sessions, sharedStore, errOut)
	if err != nil {
		return fail(err)
	}
	stops = append(stops, func() { shutdownHTTP(reverseSrv, "reverse proxy", errOut) })

	apiSrv, sessionAddr, err := startSessionAPI(cfg.Listener.SessionAPIAddr, inboundH, outboundH, sessions, serveErr, errOut)
	if err != nil {
		return fail(err)
	}
	stops = append(stops, func() {
		if apiSrv == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := apiSrv.Shutdown(ctx); err != nil {
			fmt.Fprintf(errOut, "session API shutdown: %v\n", err)
		}
	})

	// A listener that failed to bind (e.g. port in use) is a setup error: report
	// it instead of running the command in a half-configured host.
	select {
	case err := <-serveErr:
		if err != nil {
			return fail(fmt.Errorf("listener: %w", err))
		}
	default:
	}

	return &authbridgeHost{
		env:      childEnv(proxyAddr, caCertPath, errOut),
		serveErr: serveErr,
		stop:     stop,
		// containerName and admin are left empty: this path runs the pipeline in
		// this process, so there is no container, and it starts no admin server
		// (stats.address is read only by the container path, which publishes the
		// port the image serves it on).
		addrs: hostAddrs{inbound: inboundAddr, session: sessionAddr},
	}, nil
}

// applySessionServerOverride applies --sessionServer to cfg. It is a no-op
// unless the flag was set explicitly: the default exists to document where the
// session API normally listens, not to override every config that names a
// different address.
//
// When set explicitly:
//   - an empty value turns session tracking off (session.enabled = false), which
//     also takes the session API down, since the API serves the store;
//   - any other value becomes listener.session_api_addr and turns session
//     tracking on, so asking for an address cannot be silently defeated by a
//     config that disabled sessions.
func applySessionServerOverride(cmd *cobra.Command, cfg *config.Config, errOut io.Writer) {
	if !cmd.Flags().Changed("sessionServer") {
		return
	}

	enabled := execArgs.sessionServer != ""
	cfg.Session.Enabled = &enabled

	if !enabled {
		// Leave the address as-is; with sessions off, nothing reads it.
		if verbose {
			fmt.Fprintln(errOut, "--sessionServer \"\": session tracking disabled")
		}
		return
	}

	cfg.Listener.SessionAPIAddr = execArgs.sessionServer
	if verbose {
		fmt.Fprintf(errOut, "--sessionServer %s overrides listener.session_api_addr\n", execArgs.sessionServer)
	}
}

// sessionBounds resolves the session store's TTL and caps from the config,
// falling back to the same defaults the authbridge binaries use.
func sessionBounds(cfg *config.Config, errOut io.Writer) (time.Duration, int, int) {
	ttl := defaultSessionTTL
	if cfg.Session.TTL != "" {
		if d, err := time.ParseDuration(cfg.Session.TTL); err == nil {
			ttl = d
		} else {
			fmt.Fprintf(errOut, "invalid session.ttl %q, using %s: %v\n", cfg.Session.TTL, ttl, err)
		}
	}
	maxEvents := defaultSessionMaxEvents
	if cfg.Session.MaxEvents > 0 {
		maxEvents = cfg.Session.MaxEvents
	}
	maxSessions := defaultSessionMaxSessions
	if cfg.Session.MaxSessions > 0 {
		maxSessions = cfg.Session.MaxSessions
	}
	return ttl, maxEvents, maxSessions
}

// buildTLSBridge constructs the TLS-termination engine when the config has a
// non-disabled tls_bridge block, so the outbound pipeline sees decrypted HTTPS
// instead of an opaque CONNECT tunnel. It returns a nil engine when the bridge
// is absent or disabled, plus the path of the CA certificate clients must trust
// (empty when there is no bridge).
func buildTLSBridge(cfg *config.Config, errOut io.Writer) (*tlsbridge.Engine, string, error) {
	// An empty mode means disabled, matching the config's own documentation.
	if cfg.TLSBridge == nil || cfg.TLSBridge.Mode == "" || cfg.TLSBridge.Mode == tlsBridgeModeDisabled {
		return nil, "", nil
	}

	// The CA is normally an operator-mounted secret under ca_dir; generate_ca
	// mints and persists a self-signed CA there when the material is absent.
	src, generated, err := tlsbridge.EnsureFileSource(cfg.TLSBridge.CADir, cfg.TLSBridge.GenerateCA)
	if err != nil {
		return nil, "", fmt.Errorf("tls-bridge CA init: %w", err)
	}
	caCertPath := filepath.Join(cfg.TLSBridge.CADir, caCertFileName)
	if generated {
		fmt.Fprintf(errOut, "tls-bridge: generated self-signed CA in %s\n", cfg.TLSBridge.CADir)
	}

	// Extra roots for re-origination to a private-CA LLM endpoint.
	var extra []byte
	if cfg.TLSBridge.UpstreamCABundle != "" {
		if extra, err = os.ReadFile(cfg.TLSBridge.UpstreamCABundle); err != nil {
			return nil, "", fmt.Errorf("tls-bridge upstream_ca_bundle: %w", err)
		}
	}
	upstream, err := tlsbridge.NewUpstreamClient(extra)
	if err != nil {
		return nil, "", fmt.Errorf("tls-bridge upstream client: %w", err)
	}

	// A nil port set lets NewDecision apply its own defaults (443, 8443).
	var ports map[int]bool
	if len(cfg.TLSBridge.Ports) > 0 {
		ports = make(map[int]bool, len(cfg.TLSBridge.Ports))
		for _, p := range cfg.TLSBridge.Ports {
			ports[p] = true
		}
	}

	engine := &tlsbridge.Engine{
		Decision: tlsbridge.NewDecision(tlsbridge.DecisionOpts{
			Ports:     ports,
			SkipHosts: cfg.TLSBridge.PassthroughHosts,
		}),
		Term:     tlsbridge.NewTerminator(tlsbridge.NewMinter(src, tlsbridge.MinterOpts{})),
		Skip:     tlsbridge.NewSkipSet(),
		Upstream: upstream,
		CAPEM:    src.CACertPEM(),
	}
	if verbose {
		fmt.Fprintf(errOut, "tls-bridge enabled (ca_dir %s, trust %s)\n", cfg.TLSBridge.CADir, caCertPath)
	}
	return engine, caCertPath, nil
}

// startForwardProxy starts the outbound (forward) proxy when the forward role is
// active, returning the address it is reachable on and the server for shutdown.
// Both are zero when the forward role is not active.
//
// The returned address is what gets injected as the child's proxy variable, so a
// wildcard bind is reported as loopback: the child has to dial a concrete host.
func startForwardProxy(
	cfg *config.Config,
	outboundH *pipeline.Holder,
	sessions *session.Store,
	bridge *tlsbridge.Engine,
	sharedStore pipeline.SharedStore,
	errOut io.Writer,
) (string, *http.Server, error) {
	if !cfg.Listener.ActiveRoles()[config.RoleForward] {
		return "", nil, nil
	}
	// ApplyPreset fills in a per-mode default (":8081" for proxy-sidecar), so an
	// empty address here means the operator explicitly blanked it — treat that as
	// "no proxy" rather than binding an arbitrary port.
	addr := cfg.Listener.ForwardProxyAddr
	if addr == "" {
		fmt.Fprintln(errOut, "listener.forward_proxy_addr is empty; no forward proxy started")
		return "", nil, nil
	}

	srv, err := forwardproxy.NewServer(outboundH, sessions, nil)
	if err != nil {
		return "", nil, fmt.Errorf("creating forward proxy: %w", err)
	}
	skipHosts, err := skiphost.New(cfg.Listener.SkipHosts)
	if err != nil {
		return "", nil, fmt.Errorf("listener.skip_hosts: %w", err)
	}
	srv.SkipHosts = skipHosts
	srv.TLSBridge = bridge
	// Both proxies must hold the same store, or a placeholder written by an
	// inbound plugin is never resolved on the way out.
	srv.Shared = sharedStore

	// Bind the listener here rather than via runtimeutil.StartHTTPServer, which
	// keeps it internal. We need the *bound* address: with port 0 the kernel
	// assigns an arbitrary free port, and the configured ":0" is not something the
	// hosted command can dial.
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", nil, fmt.Errorf("forward-proxy listen on %s: %w", addr, err)
	}
	bound := ln.Addr().String()

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("HTTP server listening", "name", "forward-proxy", "addr", bound)
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP server failed", "name", "forward-proxy", "error", err)
		}
	}()

	if verbose {
		fmt.Fprintf(errOut, "forward proxy listening on %s\n", bound)
	}
	return proxyURL(bound), httpSrv, nil
}

// startReverseProxy starts the inbound (reverse) proxy when the reverse role is
// active, returning the server for shutdown and its bound address. Both are
// zero when the reverse role is inactive or the address was explicitly blanked.
//
// The address is returned for the instance record, not for the child: unlike
// startForwardProxy's, it is where *callers* reach the hosted service rather
// than where the hosted command sends its egress, so nothing in the child's
// environment depends on it.
//
// The listener is bound here rather than through runtimeutil.StartReverseProxyServer,
// which keeps it internal. Same reason as the forward proxy: we need the *bound*
// address, since with port 0 the kernel assigns the port and ":0" is not
// something an operator can hand to a caller. Server.Listen is still what binds,
// so the TLS-sniffing listener an mTLS config would need stays in the path — only
// the Serve call moves out here.
func startReverseProxy(
	cfg *config.Config,
	inboundH *pipeline.Holder,
	sessions *session.Store,
	sharedStore pipeline.SharedStore,
	errOut io.Writer,
) (*http.Server, string, error) {
	if !cfg.Listener.ActiveRoles()[config.RoleReverse] {
		return nil, "", nil
	}
	// ApplyPreset fills in a per-mode default (":8080" for proxy-sidecar), so an
	// empty address here means the operator explicitly blanked it — treat that as
	// "no reverse proxy" rather than binding an arbitrary port.
	addr := cfg.Listener.ReverseProxyAddr
	if addr == "" {
		fmt.Fprintln(errOut, "listener.reverse_proxy_addr is empty; no reverse proxy started")
		return nil, "", nil
	}

	backend := cfg.Listener.ReverseProxyBackend
	if err := validateBackendURL(backend); err != nil {
		return nil, "", fmt.Errorf("listener.reverse_proxy_backend: %w", err)
	}

	// mTLS is deliberately off: reverseproxy.MTLSOptions needs an X509Source from
	// a SPIFFE provider, and exec keeps that provider nil because NewProvider
	// blocks on the SPIRE Workload API. A strict config is rejected earlier, in
	// startAuthbridgeHost, so reaching here means plaintext is acceptable.
	srv, err := reverseproxy.NewServer(inboundH, sessions, backend, nil)
	if err != nil {
		return nil, "", fmt.Errorf("creating reverse proxy: %w", err)
	}
	srv.Shared = sharedStore

	ln, err := srv.Listen(addr)
	if err != nil {
		return nil, "", fmt.Errorf("reverse-proxy listen on %s: %w", addr, err)
	}
	bound := ln.Addr().String()

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		slog.Info("Reverse server listening", "name", "reverse-proxy", "addr", bound,
			"mtls", srv.MTLSEnabled())
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Reverse server failed", "name", "reverse-proxy", "error", err)
		}
	}()

	// Printed unconditionally, like the session API's address: callers of the
	// hosted service need to know where to send traffic, and slog output goes to
	// --logfile where they would have to go looking for it.
	//
	// Deliberately no wildcard warning here, unlike the session API: that
	// endpoint is unauthenticated, whereas a reverse proxy exists to receive
	// callers from elsewhere, so binding every interface is its job rather than a
	// mistake.
	fmt.Fprintf(errOut, "reverse proxy listening on %s -> %s\n", bound, backend)
	return httpSrv, bound, nil
}

// validateBackendURL rejects a reverse_proxy_backend that parses but cannot be
// proxied to.
//
// reverseproxy.NewServer only calls url.Parse, which accepts an empty string, a
// bare "localhost:8001" (whose host becomes the *scheme*), and a scheme-only
// "http://". Each yields a server that binds cleanly and then fails every
// request with a 502 that says nothing about why, so the mistake is worth
// catching at startup where it can name the field.
//
// Reachability is deliberately not checked: the backend is typically the hosted
// command itself, which has not started listening yet when the host is built, so
// a dial probe would fail on every correct invocation.
func validateBackendURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("is required when the reverse role is active")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a valid URL: %w", raw, err)
	}
	// A bare "host:port" parses with the host as the scheme, which is by far the
	// most common way to get this wrong.
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q needs an http:// or https:// scheme (got scheme %q)", raw, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("%q has no host", raw)
	}
	return nil
}

// startSessionAPI starts the session API on addr when addr is set and session
// tracking is on, returning the server and its bound address. Both are zero when
// the endpoint is disabled. Serving happens in a goroutine so the caller can
// carry on to the command; a serve failure arrives on serveErr rather than
// killing the process.
//
// The listener is bound here rather than through sessionapi's own
// ListenAndServe so the *bound* address is known: with port 0 the kernel picks
// the port, and the configured ":0" is not something an operator can curl. That
// bound address is both printed and returned, the latter for the instance
// record. A bind failure is returned synchronously, since it means the endpoint
// never came up at all.
func startSessionAPI(
	addr string,
	inboundH, outboundH *pipeline.Holder,
	sessions *session.Store,
	serveErr chan<- error,
	errOut io.Writer,
) (*sessionapi.Server, string, error) {
	if addr == "" || sessions == nil {
		return nil, "", nil
	}

	// Both holders are passed so /v1/pipeline reports what was actually
	// configured. Reporting only outbound would understate a config with an
	// inbound pipeline, which reads as "no inbound plugins" rather than "not
	// shown".
	srv := sessionapi.New(addr, sessions,
		sessionapi.WithPipelines(inboundH, outboundH),
		sessionapi.WithCatalog(sessionapi.PluginsCatalog),
	)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, "", fmt.Errorf("session API listen on %s: %w", addr, err)
	}
	bound := ln.Addr().String()

	go func() {
		if err := srv.Server().Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// The endpoint is always unauthenticated, but it is only *exposed* when bound
	// to a wildcard address; a loopback bind is reachable only from this host, so
	// warning there would be noise on every run.
	if isWildcardHost(bound) {
		slog.Warn("session API listening — UNAUTHENTICATED; may contain raw user content; do not expose beyond loopback",
			"addr", bound)
	}
	// Printed to stderr, unconditionally: the operator needs the address to use
	// the endpoint. It deliberately does not go through slog, which --logfile
	// redirects to a file the operator would have to go looking for.
	fmt.Fprintf(errOut, "session API listening on %s\n", bound)
	return srv, bound, nil
}

// isWildcardHost reports whether addr binds every interface rather than a
// specific one — an empty, 0.0.0.0, or :: host. Those are the binds that expose
// the unauthenticated session API beyond this host.
func isWildcardHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Not host:port; treat it as specific rather than crying wolf.
		return false
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return true
	}
	// An IPv6 unspecified address in another written form (e.g. "::0", "0:0:...:0")
	// is still a wildcard bind.
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return true
	}
	return false
}

// shutdownHTTP gracefully stops srv, reporting a failure without changing the
// command's exit status. A nil srv is a no-op.
func shutdownHTTP(srv *http.Server, name string, errOut io.Writer) {
	if srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Fprintf(errOut, "%s shutdown: %v\n", name, err)
	}
}

// proxyURL turns a listen address into a URL the child can dial. A wildcard or
// empty host ("" or ":8081" or "0.0.0.0:8081") is rewritten to loopback, since
// the child needs a concrete address to connect to.
func proxyURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// Not host:port — pass it through and let the child's HTTP client decide.
		return "http://" + addr
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// childEnv returns the environment for the child command: rossoctl's own, plus
// the variables that point the command at whatever was started.
//
//   - A forward proxy sets HTTP_PROXY (and its lowercase form, which many
//     clients read instead).
//   - A TLS bridge additionally sets HTTPS_PROXY — without termination, HTTPS
//     through the proxy is an opaque tunnel the pipeline cannot read — plus the
//     CA trust variables, because the bridge presents its own leaf certificate
//     and the client rejects it otherwise.
//
// Returns nil when nothing was started, which exec.Cmd treats as "inherit".
// A variable already set in rossoctl's environment is preserved: an operator who
// exported HTTPS_PROXY deliberately should not have it silently rewritten.
func childEnv(proxyAddr, caCertPath string, errOut io.Writer) []string {
	if proxyAddr == "" {
		return nil
	}

	add := map[string]string{
		"HTTP_PROXY": proxyAddr,
		"http_proxy": proxyAddr,
	}
	if caCertPath != "" {
		add["HTTPS_PROXY"] = proxyAddr
		add["https_proxy"] = proxyAddr
		// One variable per ecosystem: Node, Python-requests, and OpenSSL/Go.
		for _, k := range caTrustVars {
			add[k] = caCertPath
		}
	}

	env := os.Environ()
	for k, v := range add {
		if existing, ok := os.LookupEnv(k); ok {
			if existing != v {
				fmt.Fprintf(errOut, "keeping inherited %s=%s (not overriding with %s)\n", k, existing, v)
			}
			continue
		}
		env = append(env, k+"="+v)
	}
	return env
}

// spiffeProviderNeeded reports whether anything in cfg consumes a SPIFFE
// provider: top-level mTLS, or a plugin whose identity scheme is spiffe-based.
// Mirrors authbridge-proxy's need-driven construction, restricted to the
// outbound pipeline exec builds.
func spiffeProviderNeeded(cfg *config.Config) bool {
	if cfg.MTLS != nil {
		return true
	}
	for _, p := range cfg.Pipeline.Outbound.Plugins {
		if pluginUsesSPIFFEIdentity(p) {
			return true
		}
	}
	return false
}

// pluginUsesSPIFFEIdentity reports whether a plugin entry's config selects the
// spiffe identity scheme (identity.type=spiffe). The entry's Config is raw JSON,
// so this probes just that one field.
func pluginUsesSPIFFEIdentity(p config.PluginEntry) bool {
	if len(p.Config) == 0 {
		return false
	}
	var probe struct {
		Identity struct {
			Type string `json:"type"`
		} `json:"identity"`
	}
	if err := json.Unmarshal(p.Config, &probe); err != nil {
		// Unparseable here just means the plugin's own typed decode will fail
		// later with a precise error; don't force the provider on for it.
		return false
	}
	return probe.Identity.Type == spiffeIdentityType
}

// passthroughArgs returns the arguments that followed the "--" delimiter.
//
// Cobra records the delimiter's position in ArgsLenAtDash: it is -1 when no
// "--" was given, and otherwise the number of positional args that preceded it.
// Requiring the delimiter keeps the boundary explicit — without it, a leading
// "-x" in the command would be parsed as a flag of rossoctl's.
func passthroughArgs(cmd *cobra.Command, args []string) ([]string, error) {
	dash := cmd.ArgsLenAtDash()
	if dash < 0 {
		return nil, fmt.Errorf("no command given: separate it with %q, e.g. `rossoctl authbridge exec --config ./authbridge.yaml -- claude --help`", "--")
	}
	if dash > 0 {
		return nil, fmt.Errorf("unexpected argument %q before %q: flags for rossoctl go first, the command goes after", args[0], "--")
	}
	argv := args[dash:]
	if len(argv) == 0 {
		return nil, fmt.Errorf("no command given after %q", "--")
	}
	return argv, nil
}

// runPassthrough runs argv with rossoctl's stdio, returning an *exitCodeError
// carrying the child's status when it is non-zero.
//
// The command is not killed on a signal: SIGINT and SIGTERM from a terminal go
// to the whole foreground process group, so the child receives them directly
// and decides its own fate. We wait for it either way and then let the callers'
// deferred shutdowns run, which is what makes "shut down after a signal or the
// command's exit" one code path rather than two.
//
// A failure to start the command (not found, not executable) is a rossoctl
// error, reported as such. A command that ran and exited non-zero is not: its
// status is passed through as our own.
func runPassthrough(cmd *cobra.Command, argv, env []string, serveErr <-chan error) error {
	child := exec.Command(argv[0], argv[1:]...)
	child.Stdin = cmd.InOrStdin()
	child.Stdout = cmd.OutOrStdout()
	child.Stderr = cmd.ErrOrStderr()
	// A nil env means "inherit rossoctl's environment" (see exec.Cmd.Env).
	child.Env = env

	if verbose {
		fmt.Fprintf(cmd.ErrOrStderr(), "exec %s\n", strings.Join(argv, " "))
	}

	if err := child.Start(); err != nil {
		return fmt.Errorf("running %s: %w", argv[0], err)
	}

	// Observe signals so a SIGINT/SIGTERM aimed at rossoctl alone (e.g. `kill`
	// on just this PID) still leads to an orderly teardown rather than leaving
	// the pipeline and session API running. Stop() restores default handling.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	waitErr := make(chan error, 1)
	go func() { waitErr <- child.Wait() }()

	for {
		select {
		case err := <-waitErr:
			return exitStatus(argv, err)

		case sig := <-sigCh:
			// Forward the signal and keep waiting: the child owns the decision
			// to exit, and its status stays the one we report.
			if verbose {
				fmt.Fprintf(cmd.ErrOrStderr(), "received %s; forwarding to %s\n", sig, argv[0])
			}
			if p := child.Process; p != nil {
				_ = p.Signal(sig)
			}

		case err := <-serveErr:
			// The session API died mid-run. Report it, but only after the child
			// has finished, so its status is still what we return.
			if err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "session API: %v\n", err)
			}
			return exitStatus(argv, <-waitErr)
		}
	}
}

// exitStatus converts a Wait error into the value RunE should return: nil for a
// clean exit, an *exitCodeError carrying the status to exit with, or a wrapped
// error when the command could not be run at all.
//
// A command killed by a signal has no exit code of its own — ExitCode reports
// -1, which is not a usable status. Shells report such a death as 128+signal,
// and that is what `$?` shows, so we report the same rather than letting -1
// reach os.Exit (which would silently become 255).
func exitStatus(argv []string, err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return &exitCodeError{code: 128 + int(ws.Signal())}
		}
		return &exitCodeError{code: exitErr.ExitCode()}
	}
	return fmt.Errorf("running %s: %w", argv[0], err)
}

// materializeConfig returns a local filesystem path holding the config named by
// ref, plus a cleanup function to call when the caller is done with it.
//
// A local path is returned as-is with a no-op cleanup: exec must never delete a
// file it did not create. A remote config is fetched into a temp file, and
// cleanup removes it.
func materializeConfig(cmd *cobra.Command, ref string) (path string, cleanup func(), err error) {
	if !isHTTPURL(ref) {
		// Fail early with a clear message rather than letting config.Load report
		// the missing file, and confirm it is a file rather than a directory.
		info, statErr := os.Stat(ref)
		if statErr != nil {
			return "", func() {}, fmt.Errorf("reading config %s: %w", ref, statErr)
		}
		if info.IsDir() {
			return "", func() {}, fmt.Errorf("reading config %s: is a directory", ref)
		}
		return ref, func() {}, nil
	}

	data, err := fetchConfigURL(cmd, ref)
	if err != nil {
		return "", func() {}, err
	}

	// Keep the ".yaml" suffix so anything inspecting the path by extension
	// behaves, and create the file 0600: a config may carry credentials.
	f, err := os.CreateTemp("", "rossoctl-cortex-exec-*.yaml")
	if err != nil {
		return "", func() {}, fmt.Errorf("creating temp file for config %s: %w", ref, err)
	}
	tmp := f.Name()
	remove := func() {
		if rmErr := os.Remove(tmp); rmErr != nil && !os.IsNotExist(rmErr) {
			fmt.Fprintf(cmd.ErrOrStderr(), "removing temp config %s: %v\n", tmp, rmErr)
		}
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		remove()
		return "", func() {}, fmt.Errorf("writing temp file for config %s: %w", ref, err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", func() {}, fmt.Errorf("writing temp file for config %s: %w", ref, err)
	}

	if verbose {
		fmt.Fprintf(cmd.ErrOrStderr(), "config %s fetched to %s\n", ref, tmp)
	}
	return tmp, remove, nil
}

// isHTTPURL reports whether ref parses as an absolute http/https URL. Anything
// else — including a Windows-style "C:\..." path or a bare filename — is
// treated as a local file.
func isHTTPURL(ref string) bool {
	u, err := url.Parse(ref)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// fetchConfigURL GETs a remote config document. No credentials are sent: the
// config is not addressed to the API server, and the URL may be any host.
func fetchConfigURL(cmd *cobra.Command, configURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(cmd.Context(), http.MethodGet, configURL, nil)
	if err != nil {
		return nil, err
	}

	if verbose {
		fmt.Fprintf(cmd.ErrOrStderr(), "GET %s (exec config)\n", configURL)
	}

	resp, err := (&http.Client{Timeout: configFetchTimeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching config from %s: %w", configURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("fetching config from %s: HTTP %d", configURL, resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxConfigBytes))
	if err != nil {
		return nil, fmt.Errorf("reading config from %s: %w", configURL, err)
	}
	return data, nil
}

// setupLogging points authbridge's logging at --logfile and announces the path,
// so the operator knows where the pipeline's output went. An empty --logfile
// leaves logging on stderr, where it interleaves with the command's own output.
func setupLogging(cmd *cobra.Command) (func(), error) {
	if execArgs.logfile == "" {
		return func() {}, nil
	}
	_, closeLog, err := openLogfile(execArgs.logfile)
	if err != nil {
		return nil, err
	}
	// Printed unconditionally: a log destination the operator cannot find is
	// worse than a line of noise.
	fmt.Fprintf(cmd.ErrOrStderr(), "authbridge log: %s\n", execArgs.logfile)
	return closeLog, nil
}

// openLogfile opens (creating or appending to) path and installs it as the
// destination for authbridge's slog output, so the pipeline's, proxy's, and
// plugins' logs do not interleave with the hosted command's own stderr.
//
// The level honors the same LOG_LEVEL convention as authbridge's own
// runtimeutil.InitLogging, which cannot be reused here because it hardcodes
// os.Stderr. The previous default logger is restored on close so a later command
// in the same process is unaffected.
func openLogfile(path string) (*os.File, func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("opening logfile %s: %w", path, err)
	}

	level := slog.LevelInfo
	switch strings.ToLower(os.Getenv("LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: level})).
		With("binary", "rossoctl-cortex-exec"))

	return f, func() {
		slog.SetDefault(prev)
		_ = f.Close()
	}, nil
}

func init() {
	f := authbridgeExecCmd.Flags()
	f.StringVar(&execArgs.config, "config", "",
		"authbridge YAML config as a local file path or an http/https URL (required)")
	f.StringVar(&execArgs.logfile, "logfile", defaultLogfile,
		"file to write authbridge's log output to")
	f.StringVar(&execArgs.sessionServer, "sessionServer", defaultSessionServer,
		`address for the session API; overrides listener.session_api_addr when set. "" disables session tracking`)
	f.StringVar(&execArgs.proxyContainerImage, "proxyContainerImage", "",
		"run the pipeline in a container from this image instead of in-process")
	f.StringVar(&execArgs.instanceName, "instanceName", "",
		"name to record for this instance; a name is generated when omitted. Fails if the name is already in use in the namespace")
	f.StringVar(&execArgs.namespace, "namespace", "",
		"namespace to record this instance in; defaults to the current context's namespace, or \""+instances.DefaultNamespace+"\"")
	f.StringVar(&execArgs.inboundProtocol, "inboundProtocol", string(instances.DefaultProtocol),
		`protocol the inbound listener fronts, recorded in the instance file: "a2a" or "mcp"`)

	// The authbridge group deliberately has no --cortex flag. exec is configured
	// entirely by --config and never resolves a context, so the flag it used to
	// register had no effect on anything; an invocation passing it is now
	// rejected rather than silently ignored. --cortex remains on the cortex
	// group, where it selects a real cortex.
	authbridgeCmd := newGroup("authbridge", "Run commands behind an AuthBridge pipeline")

	authbridgeCmd.AddCommand(authbridgeExecCmd)
	rootCmd.AddCommand(authbridgeCmd)
}
