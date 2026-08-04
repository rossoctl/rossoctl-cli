package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rossoctl/cortex/authbridge/authlib/config"
)

// TestContainerCADir verifies which configs get a host CA directory mounted in.
// The decision matters both ways: without a mount a generated CA is unreachable
// by the child, and with one an operator-supplied CA would be shadowed by an
// empty temp directory.
func TestContainerCADir(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "generate_ca mounts ca_dir",
			yaml: "mode: proxy-sidecar\ntls_bridge:\n  mode: enabled\n  ca_dir: /etc/authbridge/ca\n  generate_ca: true\n",
			want: "/etc/authbridge/ca",
		},
		{
			// The CA is the operator's own material, already at ca_dir inside the
			// image or mounted by them; an empty temp dir would hide it.
			name: "operator-supplied CA is not mounted",
			yaml: "mode: proxy-sidecar\ntls_bridge:\n  mode: enabled\n  ca_dir: /etc/authbridge/ca\n  generate_ca: false\n",
			want: "",
		},
		{
			name: "no tls_bridge block",
			yaml: "mode: proxy-sidecar\n",
			want: "",
		},
		{
			// Nothing terminates TLS, so no CA is involved at all.
			name: "disabled bridge",
			yaml: "mode: proxy-sidecar\ntls_bridge:\n  mode: disabled\n  ca_dir: /etc/authbridge/ca\n  generate_ca: true\n",
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Load(writeConfig(t, tc.yaml))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			got, ok := containerCADir(cfg)
			if got != tc.want || ok != (tc.want != "") {
				t.Errorf("containerCADir = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.want != "")
			}
		})
	}
}

// TestWaitForCACertAppears verifies the wait returns the path once the container
// writes the certificate, which is the normal (slightly delayed) case.
func TestWaitForCACertAppears(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, caCertFileName)

	// Write it shortly after the wait starts, as the container would.
	go func() {
		time.Sleep(50 * time.Millisecond)
		if err := os.WriteFile(path, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600); err != nil {
			t.Errorf("WriteFile: %v", err)
		}
	}()

	got, err := waitForCACert(context.Background(), dir, io.Discard)
	if err != nil {
		t.Fatalf("waitForCACert: %v", err)
	}
	if got != path {
		t.Errorf("waitForCACert = %q, want %q", got, path)
	}
}

// TestWaitForCACertIgnoresEmptyFile verifies an empty ca.crt does not satisfy the
// wait. The bridge creates the file before writing it, so returning early would
// point the child at a truncated certificate and fail every handshake.
func TestWaitForCACertIgnoresEmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, caCertFileName)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// A short deadline stands in for the 30s budget: the point is that an empty
	// file times out rather than returning.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	if _, err := waitForCACert(ctx, dir, io.Discard); err == nil {
		t.Fatal("expected a timeout while ca.crt is still empty")
	}

	// And once it has content, the same directory succeeds.
	if err := os.WriteFile(path, []byte("cert"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := waitForCACert(context.Background(), dir, io.Discard); err != nil {
		t.Errorf("waitForCACert after write: %v", err)
	}
}

// TestWaitForCACertTimeoutMentionsPath verifies the timeout error names the file
// and the likely cause, since a ca_dir mismatch between the config and the mount
// is the way this fails in practice.
func TestWaitForCACertTimeoutMentionsPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	dir := t.TempDir()
	_, err := waitForCACert(ctx, dir, io.Discard)
	if err == nil {
		t.Fatal("expected a timeout for a directory that never gets a CA")
	}
	if !strings.Contains(err.Error(), caCertFileName) || !strings.Contains(err.Error(), "ca_dir") {
		t.Errorf("error %q should name %s and ca_dir", err, caCertFileName)
	}
}

// TestProxyContainerImageDefaultsOff verifies the flag defaults to empty, which
// is what keeps exec's in-process path the default behavior.
func TestProxyContainerImageDefaultsOff(t *testing.T) {
	f := authbridgeExecCmd.Flags().Lookup("proxyContainerImage")
	if f == nil {
		t.Fatal("--proxyContainerImage is not registered")
	}
	if f.DefValue != "" {
		t.Errorf("--proxyContainerImage default = %q, want empty", f.DefValue)
	}
}

// TestShortID verifies IDs are abbreviated the way the container CLIs display
// them, and that a short ID is passed through rather than sliced out of range.
func TestShortID(t *testing.T) {
	tests := []struct{ in, want string }{
		{"abc123def4567890abcdef", "abc123def456"},
		{"abc123def456", "abc123def456"},
		{"abc", "abc"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := shortID(tc.in); got != tc.want {
			t.Errorf("shortID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
