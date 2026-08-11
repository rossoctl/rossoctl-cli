package rossoctlclient

import (
	"net/http"
	"testing"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
	"github.com/rossoctl/rossoctl-cli/internal/config"
	"github.com/rossoctl/rossoctl-cli/internal/inprocess"
)

// TestNewClientIsAlwaysAPIClient verifies every context yields the one client
// implementation, whatever its type field says.
//
// The type field is deliberately not consulted: every context every command
// creates is type "api", and a cortex is selected by name instead. Asserting
// across the types pins that a stored "cortex" type does not reroute anything on
// its own.
func TestNewClientIsAlwaysAPIClient(t *testing.T) {
	for _, ctxType := range []config.Type{
		config.TypeAPI,
		config.TypeCortex,
		"", // unset
	} {
		t.Run(string(ctxType), func(t *testing.T) {
			c, err := NewClient(&config.Context{Type: ctxType, Server: "http://x/api/v1/"})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			if _, ok := c.(*apiclient.Client); !ok {
				t.Errorf("type %q: got %T, want *apiclient.Client", ctxType, c)
			}
		})
	}
}

// TestNewClientTransportByName verifies the name, and only the name, decides
// whether the request is served in this process or dialed.
//
// A nil HTTPClient is the assertion for the dialing case rather than an
// oversight: apiclient supplies its own default when the field is unset, so nil
// here means "apiclient's network client with its timeout."
func TestNewClientTransportByName(t *testing.T) {
	// Point at a directory-free HOME so building the in-process handler reads an
	// instances tree that does not exist, which is a successful empty read.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	for _, tc := range []struct {
		name      string
		ctxName   string
		wantLocal bool
	}{
		{name: "cortex is served in-process", ctxName: CortexContextName, wantLocal: true},
		{name: "another name dials", ctxName: "prod", wantLocal: false},
		{name: "a name containing cortex dials", ctxName: "my-cortex", wantLocal: false},
		{name: "an unnamed context dials", ctxName: "", wantLocal: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := NewClient(&config.Context{
				Type:   config.TypeAPI,
				Name:   tc.ctxName,
				Server: "http://localhost:9097/api/v1/",
			})
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			client, ok := c.(*apiclient.Client)
			if !ok {
				t.Fatalf("got %T, want *apiclient.Client", c)
			}

			if !tc.wantLocal {
				if client.HTTPClient != nil {
					t.Fatalf("HTTPClient = %#v, want nil so apiclient dials with its own default",
						client.HTTPClient)
				}
				return
			}

			if client.HTTPClient == nil {
				t.Fatal("HTTPClient is nil; a cortex context must carry the in-process transport")
			}
			if _, local := client.HTTPClient.Transport.(*inprocess.Transport); !local {
				t.Errorf("Transport = %T, want *inprocess.Transport", client.HTTPClient.Transport)
			}
		})
	}
}

// TestNewClientCarriesContextFields verifies the server and token reach the
// client for both transports.
//
// The cortex case is the interesting half: its server is not dialed, but BaseURL
// still has to carry it, because that is what apiclient builds request URLs from
// and what the handler's mount path is derived from. The token has to survive
// too, so repointing the context at a real server later does not silently lose
// it.
func TestNewClientCarriesContextFields(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	for _, tc := range []struct {
		name    string
		ctxName string
	}{
		{name: "dialing context", ctxName: "prod"},
		{name: "in-process context", ctxName: CortexContextName},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &config.Context{
				Type:        config.TypeAPI,
				Name:        tc.ctxName,
				Server:      "http://localhost:9097/api/v1/",
				BearerToken: "tok",
			}
			c, err := NewClient(ctx)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			client, ok := c.(*apiclient.Client)
			if !ok {
				t.Fatalf("got %T, want *apiclient.Client", c)
			}
			if client.BaseURL != ctx.Server {
				t.Errorf("BaseURL = %q, want %q", client.BaseURL, ctx.Server)
			}
			if client.BearerToken != ctx.BearerToken {
				t.Errorf("BearerToken = %q, want %q", client.BearerToken, ctx.BearerToken)
			}
		})
	}
}

// TestNewClientErrorsOnUnusableCortexContext verifies a cortex context whose
// server cannot yield a mount path is reported rather than quietly dialed.
//
// Falling back would send the command to whatever is listening at the default
// address, which is a different server than the user asked for.
func TestNewClientErrorsOnUnusableCortexContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	c, err := NewClient(&config.Context{
		Type:   config.TypeAPI,
		Name:   CortexContextName,
		Server: "", // no server, so there is no path to mount at
	})
	if err == nil {
		t.Fatalf("NewClient returned %#v, want an error for a cortex context with no server", c)
	}
	if c != nil {
		t.Errorf("client = %#v, want nil alongside the error", c)
	}
}

// Guard against the interface assertion above being satisfied by something that
// is not an http.RoundTripper, which would make the transport checks vacuous.
var _ http.RoundTripper = (*inprocess.Transport)(nil)
