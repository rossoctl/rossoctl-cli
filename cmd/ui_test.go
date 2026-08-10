package cmd

import (
	"strings"
	"testing"

	"github.com/rossoctl/rossoctl-cli/internal/config"
)

// stubBrowserOpener replaces browserOpener for the duration of a test, recording
// the URL it was asked to open. It restores the original on cleanup.
func stubBrowserOpener(t *testing.T) *string {
	t.Helper()
	orig := browserOpener
	var opened string
	browserOpener = func(url string) error {
		opened = url
		return nil
	}
	t.Cleanup(func() { browserOpener = orig })
	return &opened
}

func TestUIOpenUsesContextServerRoot(t *testing.T) {
	isolateHome(t)
	opened := stubBrowserOpener(t)

	// A server with a path and port: ui open must strip the path and keep the
	// scheme + host (host includes the port).
	if _, err := execute(t, "config", "create-context",
		"--name", "dev", "--server", "http://rossoctl-ui.localtest.me:8080/api/v1/",
		"--namespace", "team1"); err != nil {
		t.Fatalf("create-context: %v", err)
	}

	if _, err := execute(t, "ui", "open"); err != nil {
		t.Fatalf("ui open: %v", err)
	}
	if want := "http://rossoctl-ui.localtest.me:8080"; *opened != want {
		t.Errorf("opened %q, want %q", *opened, want)
	}
}

func TestUIOpenBlankServerFails(t *testing.T) {
	path := isolateHome(t)
	opened := stubBrowserOpener(t)

	// Seed a current context whose server is blank. create-context always fills
	// a server (falling back to the default), so write the config directly.
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Upsert(config.Context{Name: "empty", Type: config.TypeAPI})
	if err := cfg.SetCurrent("empty"); err != nil {
		t.Fatalf("set current: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}

	_, err = execute(t, "ui", "open")
	if err == nil {
		t.Fatal("ui open with a blank server should fail")
	}
	if !strings.Contains(err.Error(), "no server") {
		t.Errorf("error = %v, want it to mention a missing server", err)
	}
	if *opened != "" {
		t.Errorf("browser should not have been opened, got %q", *opened)
	}
}
