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
	"github.com/rossoctl/cortex/authbridge/authlib/listener/skiphost"
	"github.com/rossoctl/cortex/authbridge/authlib/pipeline"
	"github.com/rossoctl/cortex/authbridge/authlib/plugins"
	"github.com/rossoctl/cortex/authbridge/authlib/session"
	"github.com/rossoctl/cortex/authbridge/authlib/sessionapi"
	"github.com/rossoctl/cortex/authbridge/authlib/spiffe"
	"github.com/rossoctl/cortex/authbridge/authlib/tlsbridge"
)

// execArgs holds the `authbridge exec` flags.
var execArgs struct {
	config        string
	logfile       string
	sessionServer string
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
	Short: "Run a command with an authbridge outbound pipeline",
	Long: `Run a command hosted by an authbridge outbound pipeline.

--config is required and names an authbridge YAML config, either a local file
path or an http/https URL. A remote config is fetched into a temporary file
(config loading is path-based) and that file is removed before exec returns.

Everything after the "--" end-of-options delimiter is passed through to the
command unchanged: flags in it are the command's own, never rossoctl's. The
command is run with its stdin, stdout, and stderr connected to rossoctl's, and
rossoctl exits with the command's exit status.

While the command runs, exec hosts it behind the config's outbound plugin
pipeline:

  - The forward proxy listens on listener.forward_proxy_addr when
    listener.roles includes "forward". This is what feeds traffic through the
    pipeline, so without it the pipeline is built but never invoked.
  - The TLS bridge terminates the command's outbound TLS when tls_bridge is
    present and not disabled, so the pipeline sees decrypted HTTPS instead of an
    opaque CONNECT tunnel.
  - The session store records traffic unless session.enabled is false, and the
    session API serves it on listener.session_api_addr when that is set.
    --sessionServer overrides that address when given, and --sessionServer ""
    turns session tracking off entirely.

The command's environment points it at what was started: HTTP_PROXY when the
forward proxy runs, and additionally HTTPS_PROXY plus the CA trust variables
(NODE_EXTRA_CA_CERTS, REQUESTS_CA_BUNDLE, SSL_CERT_FILE) when the TLS bridge
runs. A variable already set in rossoctl's own environment is left alone.

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

// runCortexExec resolves the cortex context, materializes and loads the config,
// brings up the outbound pipeline and session API, runs the pass-through
// command, and mirrors its exit status.
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

	if _, err := resolveCortexContext(cmd, false); err != nil {
		return err
	}

	// Redirect authbridge's logging to the logfile before anything that logs
	// runs, and tell the operator where it went — otherwise the pipeline's own
	// output would vanish silently.
	logClose, err := setupLogging(cmd)
	if err != nil {
		return err
	}
	defer logClose()

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

	return execWithPipeline(cmd, argv, cfg)
}

// execWithPipeline brings up the whole host around the child command — outbound
// pipeline, session store, TLS bridge, forward proxy, session API — and tears it
// all down once the command exits or a signal arrives. Teardown is by deferred
// calls, so it runs in reverse construction order on every exit path, including
// a setup failure partway through.
//
// The returned error is the child's status (as an *exitCodeError) when the
// command ran, or a setup failure otherwise.
func execWithPipeline(cmd *cobra.Command, argv []string, cfg *config.Config) error {
	errOut := cmd.ErrOrStderr()

	// Apply the mode's preset and validate before building anything, so a bad
	// config is reported as such rather than as an obscure plugin error. This
	// mirrors the authbridge binaries' boot sequence.
	config.ApplyPreset(cfg)

	// --sessionServer overrides the config, so it is applied after the preset:
	// ApplyPreset substitutes ":9094" for an empty address, which would undo an
	// explicit request to turn the session API off.
	applySessionServerOverride(cmd, cfg, errOut)

	if err := config.Validate(cfg); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}

	// The SPIFFE provider is built only when something actually consumes it:
	// NewProvider blocks until the SPIRE Workload API returns the first SVID, so
	// constructing it on the mere presence of a `spiffe:` block would hang on a
	// host without SPIRE. Need-driven construction matches authbridge-proxy.
	var provider *spiffe.Provider
	if cfg.SPIFFE != nil && spiffeProviderNeeded(cfg) {
		mirrorFiles := true
		if cfg.SPIFFE.MirrorFiles != nil {
			mirrorFiles = *cfg.SPIFFE.MirrorFiles
		}
		p, err := spiffe.NewProvider(cmd.Context(), spiffe.ProviderConfig{
			SocketPath:  cfg.SPIFFE.Socket,
			MirrorFiles: mirrorFiles,
			MirrorDir:   cfg.SPIFFE.MirrorDir,
		})
		if err != nil {
			return fmt.Errorf("spiffe provider: %w", err)
		}
		defer p.Close()
		provider = p
	}

	outbound, err := plugins.BuildWithSPIFFE(cfg.Pipeline.Outbound.Plugins, provider)
	if err != nil {
		return fmt.Errorf("building outbound pipeline: %w", err)
	}

	startCtx, cancelStart := context.WithTimeout(cmd.Context(), pipelineStartTimeout)
	defer cancelStart()
	if err := outbound.Start(startCtx); err != nil {
		return fmt.Errorf("starting outbound pipeline: %w", err)
	}
	// Stop the pipeline on every exit path, including a setup failure below.
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		outbound.Stop(stopCtx)
	}()

	// Warm the plugin catalog so a factory violating the constructor contract
	// surfaces here rather than on the first /v1/plugins request.
	plugins.WarmCatalog()

	outboundH := pipeline.NewHolder(outbound)

	// The session store backs both the forward proxy's recording and the session
	// API's reads, so it is created once, before either. Session tracking
	// defaults to on; an explicit `session.enabled: false` opts out.
	var sessions *session.Store
	if cfg.Session.SessionEnabled() {
		ttl, maxEvents, maxSessions := sessionBounds(cfg, errOut)
		sessions = session.New(ttl, maxEvents, maxSessions)
		defer sessions.Close()
		if verbose {
			fmt.Fprintf(errOut, "session tracking enabled (ttl %s, maxEvents %d, maxSessions %d)\n",
				ttl, maxEvents, maxSessions)
		}
	}

	// The TLS bridge must exist before the forward proxy, which consumes it.
	// A nil engine leaves the proxy's blind-CONNECT-tunnel behavior intact.
	bridge, caCertPath, err := buildTLSBridge(cfg, errOut)
	if err != nil {
		return err
	}

	// serveErr carries an async listener failure (session API or forward proxy)
	// from its goroutine back to the command loop.
	serveErr := make(chan error, 2)

	// Start the forward proxy when the forward role is active. This is what
	// actually feeds traffic through the outbound pipeline: without it the
	// pipeline is built but never invoked.
	proxyAddr, proxySrv, err := startForwardProxy(cfg, outboundH, sessions, bridge, errOut)
	if err != nil {
		return err
	}
	defer shutdownHTTP(proxySrv, "forward proxy", errOut)

	apiSrv, err := startSessionAPI(cfg.Listener.SessionAPIAddr, outboundH, sessions, serveErr, errOut)
	if err != nil {
		return err
	}
	defer func() {
		if apiSrv == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := apiSrv.Shutdown(ctx); err != nil {
			fmt.Fprintf(errOut, "session API shutdown: %v\n", err)
		}
	}()

	// A listener that failed to bind (e.g. port in use) is a setup error: report
	// it instead of running the command in a half-configured host.
	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("listener: %w", err)
		}
	default:
	}

	return runPassthrough(cmd, argv, childEnv(proxyAddr, caCertPath, errOut), serveErr)
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

// startSessionAPI starts the session API on addr when addr is set and session
// tracking is on. Serving happens in a goroutine so the caller can carry on to
// the command; a serve failure arrives on serveErr rather than killing the
// process.
//
// The listener is bound here rather than through sessionapi's own
// ListenAndServe so the *bound* address is known: with port 0 the kernel picks
// the port, and the configured ":0" is not something an operator can curl. A
// bind failure is returned synchronously, since it means the endpoint never came
// up at all.
func startSessionAPI(
	addr string,
	outboundH *pipeline.Holder,
	sessions *session.Store,
	serveErr chan<- error,
	errOut io.Writer,
) (*sessionapi.Server, error) {
	if addr == "" || sessions == nil {
		return nil, nil
	}

	srv := sessionapi.New(addr, sessions,
		sessionapi.WithPipelines(nil, outboundH),
		sessionapi.WithCatalog(sessionapi.PluginsCatalog),
	)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("session API listen on %s: %w", addr, err)
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
	return srv, nil
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

	authbridgeCmd := newGroup("authbridge", "Run commands behind an AuthBridge pipeline")

	// exec resolves a cortex-typed context (see resolveCortexContext), which is
	// named by --cortex. That flag lives on the cortex group, so authbridge
	// registers its own copy rather than inheriting one: both bind the same
	// cortexName variable, so the two groups behave identically.
	authbridgeCmd.PersistentFlags().StringVar(&cortexName, "cortex", defaultCortexName,
		"name of the cortex to operate on")
	// Deprecated: authbridge exec is configured by --config, not by a cortex.
	// MarkDeprecated also hides the flag; it keeps working, and pflag prints the
	// message when it is used.
	if err := authbridgeCmd.PersistentFlags().MarkDeprecated("cortex",
		"authbridge exec is configured by --config; --cortex has no effect on the pipeline"); err != nil {
		// A failure here means the flag name above is wrong — a programming
		// error, and one that would otherwise pass silently.
		panic(fmt.Sprintf("marking --cortex deprecated: %v", err))
	}

	authbridgeCmd.AddCommand(authbridgeExecCmd)
	rootCmd.AddCommand(authbridgeCmd)
}
