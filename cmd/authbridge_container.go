package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/rossoctl/cortex/authbridge/authlib/config"

	"github.com/rossoctl/rossoctl-cli/internal/containers"
)

// Container ports the authbridge proxy image publishes. They are the image's
// own fixed ports; each is published on an ephemeral host port, and the host
// side is discovered with Inspect rather than assumed.
//
// TODO: derive the forward proxy's container port from the port in the config's
// listener.forward_proxy_addr instead of always using 8081. The image listens
// wherever that address says, so a config naming another port would leave the
// child pointed at a port nothing is on.
const (
	containerReversePort      = 8080
	containerForwardProxyPort = 8081
	containerAdminPort        = 9093
	containerSessionAPIPort   = 9094
)

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
func startAuthbridgeContainer(cmd *cobra.Command, cfg *config.Config, cfgPath, image string) (*authbridgeHost, error) {
	errOut := cmd.ErrOrStderr()

	engine, bin, err := containers.Detect()
	if err != nil {
		return nil, err
	}
	if verbose {
		fmt.Fprintf(errOut, "using container runtime %s\n", bin)
	}

	pc, err := runProxyContainer(cmd, engine, cfg, cfgPath, image)
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
	// publishes fixed ports; the host side is only knowable after the fact.
	ports, err := engine.Inspect(cmd.Context(), pc.id)
	if err != nil {
		return fail(fmt.Errorf("inspecting proxy container: %w", err))
	}

	proxyPort, ok := containers.HostPort(ports, containerForwardProxyPort)
	if !ok {
		return fail(fmt.Errorf("proxy container published no host port for %d/tcp; "+
			"does image %s expose the forward proxy there?", containerForwardProxyPort, image))
	}
	proxyAddr := proxyURL(fmt.Sprintf("127.0.0.1:%d", proxyPort))

	if verbose {
		reportContainerPorts(errOut, ports)
	}
	// Printed unconditionally, like the in-process session API address: the
	// operator needs the session API port to use the endpoint, and it is
	// different on every run.
	if p, ok := containers.HostPort(ports, containerSessionAPIPort); ok {
		fmt.Fprintf(errOut, "session API listening on 127.0.0.1:%d (in container %s)\n", p, shortID(pc.id))
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
	}, nil
}

// runProxyContainer prepares the CA directory (when the config asks the bridge
// to generate one) and starts the container. On a start failure the temp
// directory is removed before returning, so a failed attempt leaves nothing
// behind.
func runProxyContainer(
	cmd *cobra.Command,
	engine containers.Engine,
	cfg *config.Config,
	cfgPath, image string,
) (*proxyContainer, error) {
	pc := &proxyContainer{engine: engine, cleanup: func() {}}

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

	id, err := engine.Start(cmd.Context(), containers.RunSpec{
		Image:        image,
		PublishPorts: []int{containerReversePort, containerForwardProxyPort, containerAdminPort, containerSessionAPIPort},
		Mounts:       mounts,
		Args:         []string{"--config", containerConfigPath},
	})
	if err != nil {
		pc.cleanup()
		return nil, fmt.Errorf("starting proxy container from %s: %w", image, err)
	}
	pc.id = id

	if verbose {
		fmt.Fprintf(cmd.ErrOrStderr(), "started proxy container %s from %s\n", shortID(id), image)
	}
	return pc, nil
}

// stopAuthbridgeContainer stops the container started by
// startAuthbridgeContainer and removes its temp CA directory.
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
func reportContainerPorts(errOut io.Writer, ports map[string][]containers.PortBinding) {
	for _, p := range []struct {
		port int
		name string
	}{
		{containerReversePort, "reverse proxy"},
		{containerForwardProxyPort, "forward proxy"},
		{containerAdminPort, "admin"},
		{containerSessionAPIPort, "session API"},
	} {
		if host, ok := containers.HostPort(ports, p.port); ok {
			fmt.Fprintf(errOut, "container %s: %d -> 127.0.0.1:%d\n", p.name, p.port, host)
		}
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
