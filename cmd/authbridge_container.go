package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/rossoctl/cortex/authbridge/authlib/config"

	"github.com/rossoctl/rossoctl-cli/internal/containers"
)

// containerPorts are the ports the proxy image listens on inside the container,
// read from the config the container is given. Each is published on an ephemeral
// host port, and the host side is discovered with Inspect rather than assumed.
//
// A zero port means "not listening", and is not published: the config either
// disabled that listener or left its role inactive, so publishing it would map a
// host port to nothing.
type containerPorts struct {
	// reverse is where callers reach the hosted service, from
	// listener.reverse_proxy_addr.
	reverse int

	// forward is the egress proxy the child's HTTP_PROXY points at, from
	// listener.forward_proxy_addr. It is the one port whose absence is fatal.
	forward int

	// admin is the stats/config endpoint, from stats.address.
	admin int

	// sessionAPI is the session events endpoint, from listener.session_api_addr.
	sessionAPI int
}

// publishList returns the ports to publish, skipping those not listening.
func (p containerPorts) publishList() []int {
	var ports []int
	for _, n := range []int{p.reverse, p.forward, p.admin, p.sessionAPI} {
		if n > 0 {
			ports = append(ports, n)
		}
	}
	return ports
}

// containerPortsFromConfig reads the ports the image will listen on out of cfg.
//
// The image binds whatever its config says, so these cannot be constants: a
// config naming other ports would have the reverse port published on nothing and
// the child pointed at a forward proxy port nothing is on. cfg must already have
// been through config.ApplyPreset (startHost does this), which is what fills the
// per-mode defaults — otherwise an omitted address reads as "not listening" when
// the image will in fact bind the preset's port.
//
// An address that is set but unparseable is an error rather than a skip: it means
// the operator asked for a listener that neither this nor the image can honor,
// and a silently unpublished port becomes a connection refused much later.
func containerPortsFromConfig(cfg *config.Config) (containerPorts, error) {
	var p containerPorts

	// Only an active role's listener is started by the image, so an address left
	// over from an inactive role must not be published. This mirrors
	// ApplyPreset, which likewise fills an address only for an active role.
	roles := cfg.Listener.ActiveRoles()

	for _, f := range []struct {
		name   string
		addr   string
		active bool
		out    *int
	}{
		{"listener.reverse_proxy_addr", cfg.Listener.ReverseProxyAddr, roles[config.RoleReverse], &p.reverse},
		{"listener.forward_proxy_addr", cfg.Listener.ForwardProxyAddr, roles[config.RoleForward], &p.forward},
		{"stats.address", cfg.Stats.StatsAddress, true, &p.admin},
		{"listener.session_api_addr", cfg.Listener.SessionAPIAddr, true, &p.sessionAPI},
	} {
		if !f.active || f.addr == "" {
			continue
		}
		port, err := listenPort(f.addr)
		if err != nil {
			return containerPorts{}, fmt.Errorf("%s %q: %w", f.name, f.addr, err)
		}
		*f.out = port
	}

	// Without a forward proxy there is nothing to point the child at, which is
	// the whole purpose of hosting the pipeline. Caught here rather than after
	// the container is running, where it costs a start and a stop to learn.
	if p.forward == 0 {
		return containerPorts{}, fmt.Errorf(
			"listener.forward_proxy_addr names no port to publish; " +
				"the hosted command has no proxy to use")
	}
	return p, nil
}

// listenPort extracts the port from a listen address ("host:port", ":port").
//
// Port 0 is rejected even though it is a legal listen address: it tells the
// image's kernel to pick a port, which cannot be published because the number is
// not known until the image has already bound it — and it is inside the
// container, where Inspect reports only what was published.
func listenPort(addr string) (int, error) {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, fmt.Errorf("not a host:port address: %w", err)
	}
	port, err := net.LookupPort("tcp", portStr)
	if err != nil {
		return 0, fmt.Errorf("invalid port %q", portStr)
	}
	if port == 0 {
		return 0, fmt.Errorf("port 0 lets the container pick a port, which cannot be published")
	}
	return port, nil
}

// containerConfigPath is where the realized config is mounted inside the
// container, and what the container's command points at with --config.
const containerConfigPath = "/tmp/config.yaml"

// caWaitTimeout bounds the wait for the container's TLS bridge to mint its CA
// into the mounted directory. Generating a key and self-signing takes well under
// a second; the budget is for image startup on a cold cache.
const caWaitTimeout = 30 * time.Second

// caPollInterval is how often the mounted CA directory is checked for ca.crt.
// A bind mount gives no change notification that is portable across Docker
// Desktop's VM boundary, so this polls.
const caPollInterval = 100 * time.Millisecond

// proxyContainer is a running proxy container and the temporary state that
// belongs to it.
type proxyContainer struct {
	// engine is what started it and what will stop it.
	engine containers.Engine

	// id identifies the container to the runtime.
	id string

	// name is the name the container was started under. Unlike id it is chosen
	// by rossoctl rather than assigned by the runtime, which is what lets an
	// instance record name a container an operator can then find in `docker ps`.
	name string

	// caDir is the host directory bind-mounted at the config's tls_bridge.ca_dir
	// for the container to write its CA into, or "" when no CA was expected.
	// It is a temp directory owned by this struct and removed by cleanup.
	caDir string

	// cleanup removes the temp CA directory. Never nil.
	cleanup func()
}

// startAuthbridgeContainer starts the proxy container named by
// --proxyContainerImage and returns the host description for the child command:
// the environment pointing it at the container's published ports, and the
// teardown that stops the container.
//
// It is the container-hosted counterpart of startAuthbridgeHost: the pipeline
// runs in the container rather than in this process, so nothing is built here —
// the config is mounted in and the image does the work.
//
// cfgPath is the realized config file on this host (see materializeConfig); it
// is mounted read-only at containerConfigPath. cfg is the parsed form of that
// same file, read here only to decide whether a CA directory is needed.
//
// name is what to call the container. It is supplied rather than left to the
// runtime so the instance record can name a container an operator can find.
func startAuthbridgeContainer(
	cmd *cobra.Command,
	cfg *config.Config,
	cfgPath, image, name string,
) (*authbridgeHost, error) {
	errOut := cmd.ErrOrStderr()

	engine, bin, err := containers.Detect()
	if err != nil {
		return nil, err
	}
	if verbose {
		fmt.Fprintf(errOut, "using container runtime %s\n", bin)
		// Log each runtime invocation to stderr, so verbose output stays clear of
		// whatever the hosted command writes to stdout.
		containers.SetLogf(engine, func(format string, args ...any) {
			fmt.Fprintf(errOut, format+"\n", args...)
		})
	}

	// Read the ports out of the config before starting anything: the image binds
	// what its config says, and a bad address is cheaper to report now than as a
	// container that starts and then cannot be reached.
	ports, err := containerPortsFromConfig(cfg)
	if err != nil {
		return nil, err
	}

	pc, err := runProxyContainer(cmd, engine, cfg, cfgPath, image, name, ports)
	if err != nil {
		return nil, err
	}

	// From here on the container is running, so any failure has to stop it
	// before returning — otherwise a setup error leaks a container.
	fail := func(err error) (*authbridgeHost, error) {
		stopAuthbridgeContainer(cmd, pc)
		return nil, err
	}

	// Discover the ephemeral host ports the runtime assigned. The container
	// publishes the ports read from its config; the host side is only knowable
	// after the fact.
	bound, err := engine.Inspect(cmd.Context(), pc.id)
	if err != nil {
		return fail(fmt.Errorf("inspecting proxy container: %w", err))
	}

	proxyPort, ok := containers.HostPort(bound, ports.forward)
	if !ok {
		return fail(fmt.Errorf("proxy container published no host port for %d/tcp; "+
			"does image %s listen on the forward proxy port its config names?", ports.forward, image))
	}
	proxyAddr := proxyURL(fmt.Sprintf("127.0.0.1:%d", proxyPort))

	if verbose {
		reportContainerPorts(errOut, bound, ports)
	}
	// The addresses to record for this instance, as reached from this host: the
	// published side of each mapping, not the port bound inside the container.
	// A port the config did not ask for, or that the image did not publish, is
	// left empty — the record distinguishes "no such listener" from an address.
	addrs := hostAddrs{containerName: pc.name}
	for _, m := range []struct {
		port int
		out  *string
	}{
		{ports.reverse, &addrs.inbound},
		{ports.sessionAPI, &addrs.session},
		{ports.admin, &addrs.admin},
	} {
		if m.port == 0 {
			continue
		}
		if host, ok := containers.HostPort(bound, m.port); ok {
			*m.out = fmt.Sprintf("127.0.0.1:%d", host)
		}
	}

	// Printed unconditionally, like the in-process session API address: the
	// operator needs the session API port to use the endpoint, and it is
	// different on every run.
	if addrs.session != "" {
		fmt.Fprintf(errOut, "session API listening on %s (in container %s)\n", addrs.session, pc.name)
	}

	// The child must trust the bridge's CA before it can talk HTTPS through it,
	// so wait for the container to mint it. Without the file the child would
	// fail every TLS handshake, which is worse than a slow start.
	var caCertPath string
	if pc.caDir != "" {
		caCertPath, err = waitForCACert(cmd.Context(), pc.caDir, errOut)
		if err != nil {
			return fail(err)
		}
	}

	// serveErr is never written to in the container case: the listeners live in
	// the container, and its death is not something this process observes. It
	// exists because runPassthrough selects on it — a nil channel would block
	// forever, which is the intended behavior here, but a non-nil empty channel
	// says so without relying on nil-channel semantics.
	serveErr := make(chan error)

	return &authbridgeHost{
		env:      childEnv(proxyAddr, caCertPath, errOut),
		serveErr: serveErr,
		stop:     func() { stopAuthbridgeContainer(cmd, pc) },
		addrs:    addrs,
	}, nil
}

// runProxyContainer prepares the CA directory (when the config asks the bridge
// to generate one) and starts the container. On a start failure the temp
// directory is removed before returning, so a failed attempt leaves nothing
// behind.
//
// ports says which container ports to publish; see containerPortsFromConfig.
// name is what to call the container.
func runProxyContainer(
	cmd *cobra.Command,
	engine containers.Engine,
	cfg *config.Config,
	cfgPath, image, name string,
	ports containerPorts,
) (*proxyContainer, error) {
	pc := &proxyContainer{engine: engine, name: name, cleanup: func() {}}

	// The config is mounted read-only: the container reads it at startup and has
	// no reason to write back to this host's file, which may be the operator's
	// own config rather than a temp copy.
	abscfg, err := filepath.Abs(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("resolving config path %s: %w", cfgPath, err)
	}
	mounts := []containers.Mount{{
		HostPath:      abscfg,
		ContainerPath: containerConfigPath,
		ReadOnly:      true,
	}}

	// A generate_ca bridge mints its CA into ca_dir at startup. Bind a host temp
	// directory there so the certificate lands where this process can read it and
	// point the child at it; without the mount the CA would exist only inside the
	// container, where the child cannot reach it.
	if caDir, ok := containerCADir(cfg); ok {
		tmp, err := os.MkdirTemp("", "rossoctl-authbridge-ca-*")
		if err != nil {
			return nil, fmt.Errorf("creating temp CA directory: %w", err)
		}
		pc.caDir = tmp
		pc.cleanup = func() {
			if err := os.RemoveAll(tmp); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "removing temp CA directory %s: %v\n", tmp, err)
			}
		}
		// Writable, unlike the config: generating the CA *is* a write, and it
		// has to reach this host.
		mounts = append(mounts, containers.Mount{HostPath: tmp, ContainerPath: caDir})
		if verbose {
			fmt.Fprintf(cmd.ErrOrStderr(), "mounting temp CA directory %s at %s\n", tmp, caDir)
		}
	}

	// Host entries are resolved before the start, so an unresolvable name in the
	// config fails without leaving a container behind. pc.cleanup covers the temp
	// CA directory the block above may have created.
	hostEntries, err := proxyContainerHostEntries(cfg, cmd.ErrOrStderr())
	if err != nil {
		pc.cleanup()
		return nil, err
	}

	id, err := engine.Start(cmd.Context(), containers.RunSpec{
		Image:        image,
		Name:         name,
		PublishPorts: ports.publishList(),
		Mounts:       mounts,
		HostEntries:  hostEntries,
		Args:         []string{"--config", containerConfigPath},
	})
	if err != nil {
		pc.cleanup()
		return nil, fmt.Errorf("starting proxy container from %s: %w", image, err)
	}
	pc.id = id

	if verbose {
		fmt.Fprintf(cmd.ErrOrStderr(), "started proxy container %s (%s) from %s\n",
			name, shortID(id), image)
	}
	return pc, nil
}

// proxyContainerHostEntries returns the extra /etc/hosts entries the proxy
// container needs so the names in its config resolve the same way they do here.
//
// A hostname in the config that resolves to loopback on this host means *this
// host* — but inside the container loopback is the container, so the pipeline
// would connect to itself and fail. Those names are mapped to host-gateway, the
// literal both runtimes special-case for the host. The local demo's
// keycloak.localtest.me is the motivating case (jwt-validation and
// token-exchange both reach a Keycloak), but nothing here is specific to it.
//
// Only loopback names are mapped. A hostname that resolves to a real address is
// reachable from the container as-is, and redirecting it to the host would break
// a pipeline pointed at a remote Keycloak.
//
// An unresolvable hostname is an error. It cannot be classified — and it is
// almost certainly a typo or a missing /etc/hosts entry that would surface as a
// container failing every token operation, which is far harder to read than this.
func proxyContainerHostEntries(cfg *config.Config, errOut io.Writer) ([]containers.HostEntry, error) {
	names := configHostnames(cfg)

	entries := make([]containers.HostEntry, 0, len(names))
	for _, name := range names {
		loopback, err := resolvesToLoopback(name)
		if err != nil {
			return nil, fmt.Errorf("resolving %s from the pipeline config: %w "+
				"(the proxy container needs it to resolve, since it reaches it by name)", name, err)
		}
		if !loopback {
			// Reachable from the container as-is; an entry would only get in the way.
			if verbose {
				fmt.Fprintf(errOut, "container host entry: %s is not loopback, no mapping needed\n", name)
			}
			continue
		}
		entries = append(entries, containers.HostEntry{Name: name, Address: containers.HostGateway})
		if verbose {
			fmt.Fprintf(errOut, "container host entry: %s -> %s (loopback on this host)\n",
				name, containers.HostGateway)
		}
	}
	return entries, nil
}

// lookupIPAddr resolves a hostname to its addresses. It is a package variable so
// tests can answer from a table instead of depending on the DNS of whatever
// machine they run on.
var lookupIPAddr = defaultLookupIPAddr

// defaultLookupIPAddr is the real resolver, bounded by hostLookupTimeout.
func defaultLookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

// hostLookupTimeout bounds a single hostname lookup. Generous for a local
// resolver or /etc/hosts, while keeping an unreachable DNS server from hanging
// the command before the container has even started.
const hostLookupTimeout = 5 * time.Second

// resolvesToLoopback reports whether name resolves to a loopback address on this
// host.
//
// A name resolving to a mix of loopback and non-loopback addresses counts as
// loopback: the container cannot reach the loopback half, which is the half a
// local service is on.
func resolvesToLoopback(name string) (bool, error) {
	// An IP literal in the config is not a hostname and cannot be an /etc/hosts
	// entry, so classify it directly rather than asking the resolver.
	if ip := net.ParseIP(name); ip != nil {
		return ip.IsLoopback(), nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), hostLookupTimeout)
	defer cancel()

	addrs, err := lookupIPAddr(ctx, name)
	if err != nil {
		return false, err
	}
	if len(addrs) == 0 {
		return false, fmt.Errorf("no addresses")
	}
	for _, a := range addrs {
		if a.IP.IsLoopback() {
			return true, nil
		}
	}
	return false, nil
}

// configHostnames returns the distinct hostnames the pipeline's plugins will
// reach, in a stable order.
//
// Plugin config is json.RawMessage — opaque by design, since each plugin owns its
// schema — so rather than knowing every plugin's fields, this walks the decoded
// JSON and collects the host of anything that parses as an absolute URL. That
// picks up keycloak_url and issuer without naming them, and any future plugin's
// URL field for free.
func configHostnames(cfg *config.Config) []string {
	var names []string
	seen := map[string]bool{}

	for _, stage := range []config.PipelineStageConfig{cfg.Pipeline.Inbound, cfg.Pipeline.Outbound} {
		for _, p := range stage.Plugins {
			if len(p.Config) == 0 {
				continue
			}
			var decoded any
			if err := json.Unmarshal(p.Config, &decoded); err != nil {
				// Not our error to report: the plugin's own Configure validates
				// its config, and the container is what runs it.
				continue
			}
			for _, h := range urlHosts(decoded) {
				if !seen[h] {
					seen[h] = true
					names = append(names, h)
				}
			}
		}
	}
	return names
}

// dialableSchemes are the URL schemes whose host is an endpoint the pipeline will
// connect to.
//
// Restricted to these rather than accepting any scheme because not every URL-shaped
// config value names something to dial. A SPIFFE ID like
// spiffe://localtest.me/ns/team1/sa/weather-service is an identity to compare
// against, and its "host" is a trust domain, not a server — the audience field in
// the local weather demo is exactly this. Mapping a trust domain to host-gateway
// would add a bogus /etc/hosts entry, and worse, an unreachable trust domain would
// fail the whole command under the unresolvable-name rule.
var dialableSchemes = map[string]bool{"http": true, "https": true}

// urlHosts walks a decoded JSON value and returns the hostnames of every string
// that is a URL the pipeline would dial, in encounter order.
//
// A host and a dialable scheme are both required. A bare string like "passthrough"
// or a path is not a URL to anything, and url.Parse accepts both without complaint —
// demanding a scheme and host is what keeps arbitrary config values out.
func urlHosts(v any) []string {
	switch t := v.(type) {
	case string:
		u, err := url.Parse(t)
		if err != nil || !dialableSchemes[u.Scheme] || u.Hostname() == "" {
			return nil
		}
		return []string{u.Hostname()}
	case []any:
		var out []string
		for _, e := range t {
			out = append(out, urlHosts(e)...)
		}
		return out
	case map[string]any:
		// Sorted for a deterministic entry order: Go randomizes map iteration,
		// and the --add-host order would otherwise vary between runs, making the
		// verbose output and the tests unstable.
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		var out []string
		for _, k := range keys {
			out = append(out, urlHosts(t[k])...)
		}
		return out
	}
	return nil
}

// stopAuthbridgeContainer stops and removes the container started by
// startAuthbridgeContainer, and removes its temp CA directory.
//
// Stopping is also what deletes the container: it is not run with --rm, so one
// that crashed stays around holding its logs until here. Anything wanting to
// report those logs must therefore do it before this runs.
//
// Failures are reported but do not change the command's exit status: the child
// has already run, and its status is what the operator asked for. The CA
// directory is removed even if the stop fails — it holds only a generated CA
// that is useless once the container is going away.
func stopAuthbridgeContainer(cmd *cobra.Command, pc *proxyContainer) {
	if pc == nil {
		return
	}
	defer pc.cleanup()

	if pc.id == "" {
		return
	}
	if err := pc.engine.Stop(cmd.Context(), pc.id); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "stopping proxy container %s: %v\n", shortID(pc.id), err)
		return
	}
	if verbose {
		fmt.Fprintf(cmd.ErrOrStderr(), "stopped proxy container %s\n", shortID(pc.id))
	}
}

// containerCADir returns the container path to bind the host CA directory at,
// and whether a CA directory is needed at all.
//
// It is needed only when the bridge is active and generating its own CA: with
// generate_ca false the CA is operator-supplied material that already exists at
// ca_dir, which is the operator's to mount, not ours to replace with an empty
// temp directory.
func containerCADir(cfg *config.Config) (string, bool) {
	if cfg.TLSBridge == nil || !cfg.TLSBridge.GenerateCA {
		return "", false
	}
	if cfg.TLSBridge.Mode == "" || cfg.TLSBridge.Mode == tlsBridgeModeDisabled {
		return "", false
	}
	if cfg.TLSBridge.CADir == "" {
		return "", false
	}
	return cfg.TLSBridge.CADir, true
}

// waitForCACert waits for the container's TLS bridge to write ca.crt into the
// mounted directory and returns its host path.
//
// The file is polled rather than watched: the directory is a bind mount, which
// on Docker Desktop crosses a VM boundary where filesystem events are not
// reliably delivered. A non-empty file is required, not merely an existing one —
// the bridge creates it before writing, so an empty file means "not yet".
func waitForCACert(ctx context.Context, caDir string, errOut io.Writer) (string, error) {
	path := filepath.Join(caDir, caCertFileName)

	waitCtx, cancel := context.WithTimeout(ctx, caWaitTimeout)
	defer cancel()

	ticker := time.NewTicker(caPollInterval)
	defer ticker.Stop()

	announced := false
	for {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			if verbose {
				fmt.Fprintf(errOut, "proxy container CA ready at %s\n", path)
			}
			return path, nil
		}

		select {
		case <-waitCtx.Done():
			return "", fmt.Errorf("waiting for %s from the proxy container: %w "+
				"(does the config's tls_bridge.ca_dir match the mounted path?)", path, waitCtx.Err())
		case <-ticker.C:
			// Say something once if this is taking long enough to notice, so a
			// pause here does not look like a hang.
			if !announced && verbose {
				fmt.Fprintf(errOut, "waiting for the proxy container's CA at %s\n", path)
				announced = true
			}
		}
	}
}

// reportContainerPorts prints the container's published port map, so an operator
// can reach the admin and session endpoints on their ephemeral host ports.
//
// bound is what Inspect reported; want is what the config asked to publish, which
// is what names each port. A port in want with no binding in bound is reported as
// unpublished rather than omitted: that gap is the interesting case, since it
// means the image did not listen where its config said.
func reportContainerPorts(errOut io.Writer, bound map[string][]containers.PortBinding, want containerPorts) {
	for _, p := range []struct {
		port int
		name string
	}{
		{want.reverse, "reverse proxy"},
		{want.forward, "forward proxy"},
		{want.admin, "admin"},
		{want.sessionAPI, "session API"},
	} {
		if p.port == 0 {
			continue
		}
		if host, ok := containers.HostPort(bound, p.port); ok {
			fmt.Fprintf(errOut, "container %s: %d -> 127.0.0.1:%d\n", p.name, p.port, host)
			continue
		}
		fmt.Fprintf(errOut, "container %s: %d not published\n", p.name, p.port)
	}
}

// shortID abbreviates a container ID to the 12-character form the container CLIs
// display, so log lines match what `docker ps` shows.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
