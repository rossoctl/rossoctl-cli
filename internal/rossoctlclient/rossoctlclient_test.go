package rossoctlclient

import (
	"testing"

	"github.com/rossoctl/rossoctl-cli/internal/apiclient"
	"github.com/rossoctl/rossoctl-cli/internal/config"
)

// TestNewClientIsAlwaysHTTP verifies every context type yields the HTTP client.
// A cortex context is no exception: it is reached by pointing it at a
// `rossoctl cortex serve` address, not by a separate file-backed client.
func TestNewClientIsAlwaysHTTP(t *testing.T) {
	for _, ctxType := range []config.Type{
		config.TypeAPI,
		config.TypeCortex,
		"", // unset
	} {
		t.Run(string(ctxType), func(t *testing.T) {
			c := NewClient(&config.Context{Type: ctxType, Server: "http://x/api/v1/"})
			if _, ok := c.(*apiclient.Client); !ok {
				t.Errorf("type %q: got %T, want *apiclient.Client", ctxType, c)
			}
		})
	}
}

func TestNewClientCarriesContextFields(t *testing.T) {
	ctx := &config.Context{Type: config.TypeAPI, Server: "http://api/", BearerToken: "tok"}
	c, ok := NewClient(ctx).(*apiclient.Client)
	if !ok {
		t.Fatalf("expected *apiclient.Client, got %T", c)
	}
	if c.BaseURL != ctx.Server {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, ctx.Server)
	}
	if c.BearerToken != ctx.BearerToken {
		t.Errorf("BearerToken = %q, want %q", c.BearerToken, ctx.BearerToken)
	}

	// A cortex context's server is honored the same way, so a context pointed at
	// a local `cortex serve` reaches it rather than being routed elsewhere.
	cortex := &config.Context{Type: config.TypeCortex, Name: "mycortex", Server: "http://localhost:9097/api/v1/"}
	cc, ok := NewClient(cortex).(*apiclient.Client)
	if !ok {
		t.Fatalf("expected *apiclient.Client for a cortex context, got %T", cc)
	}
	if cc.BaseURL != cortex.Server {
		t.Errorf("BaseURL = %q, want %q", cc.BaseURL, cortex.Server)
	}
}
