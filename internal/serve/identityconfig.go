package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/rossoctl/rossoctl-cli/internal/instances"
)

// adminConfigPath is where AuthBridge serves its running configuration: the
// /config endpoint of authlib's stat server, which marshals the live config
// object on each request.
const adminConfigPath = "/config"

// identityConfigTimeout bounds the fetch from the instance's admin endpoint,
// matching agentCardTimeout for the same reason — the listener is on this host
// over loopback, so a healthy one answers immediately and the budget exists for
// one that accepted the connection and then stalled.
const identityConfigTimeout = 10 * time.Second

// configFetcher fetches the configuration document at the given URL. A variable
// so a test can answer without a live admin listener, following cardFetcher;
// production always uses the real HTTP client.
var configFetcher = fetchIdentityConfig

// agentIdentityConfigRoute serves GET /agents/{namespace}/{name}/identity-config
// by fetching the configuration from the AuthBridge instance's admin endpoint.
//
// Like the agent card, this cannot be answered from the record: the record says
// where AuthBridge is, not how it was configured. The address comes from the
// record's admin_addr — the stats/config listener — and the body is whatever that
// listener serves at /config, which is the configuration AuthBridge is actually
// running from rather than the file it was started with.
//
// The body is forwarded byte for byte rather than decoded and re-encoded. Two
// reasons, both load-bearing. The upstream pipes its config through authlib's
// redaction before serving it, so the bytes arriving here already have their
// secret-looking values replaced; re-marshaling would reshape a payload whose
// safety property was established upstream. And this server models only part of
// the config — decoding into a struct would silently drop every block it does not
// know about, which is most of a real configuration.
//
// That inherits the upstream's redaction exactly, and no more: authlib documents
// it as best-effort defense-in-depth keyed on field names, with the real defense
// being keeping inline secrets out of the config at all. A secret its heuristic
// misses is served here too.
//
// The record is resolved through the same lookup the detail endpoint uses, so this
// 404s exactly when that does, including for an mcp instance.
func agentIdentityConfigRoute(opts) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		inst, ok := lookupInstance(w, r, instances.ProtocolA2A)
		if !ok {
			return
		}

		// No admin listener means there is nothing to ask, and this is the common
		// case rather than a fault: `authbridge exec` starts an admin server only
		// on the container path, so an in-process instance is healthy and simply
		// has no endpoint. 503 for the same reason the card endpoint uses one —
		// the instance exists, so 404 would contradict the detail endpoint — but
		// the message says what is missing rather than reporting it unreachable.
		if inst.AdminAddr == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"detail": fmt.Sprintf("instance %q is running without an admin listener, so its configuration cannot be read; only container-hosted instances expose one", inst.Name),
			})
			return
		}

		configURL := "http://" + inst.AdminAddr + adminConfigPath
		body, status, err := configFetcher(r.Context(), configURL)
		if err != nil {
			// Unreachable: the record is advisory, so a stale admin address lands
			// here rather than yielding a stale configuration. The address is named
			// because it is what identifies which listener is down.
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{
				"detail": fmt.Sprintf("failed to connect to the admin endpoint at %s: %v", inst.AdminAddr, err),
			})
			return
		}
		if status < 200 || status >= 300 {
			// The admin endpoint's own status is passed through, as the card
			// endpoint does: flattening it would hide which of the two hops
			// refused, and a 404 here means the listener is not an AuthBridge
			// admin endpoint at all.
			writeJSON(w, status, map[string]string{
				"detail": fmt.Sprintf("failed to read configuration from %s: admin endpoint returned %d", inst.AdminAddr, status),
			})
			return
		}

		// Checked before the header goes out, not after. writeJSON cannot report an
		// encoding failure once the status is committed, so forwarding unchecked
		// bytes would answer 200 with a body a caller cannot parse — or with none
		// at all. A caller asked for a configuration, so a body that is not a JSON
		// object is a bad answer (502) rather than a successful one.
		if !isJSONObject(body) {
			writeJSON(w, http.StatusBadGateway, map[string]string{
				"detail": fmt.Sprintf("admin endpoint at %s served an unreadable configuration", inst.AdminAddr),
			})
			return
		}

		writeJSONBytes(w, http.StatusOK, body)
	}
}

// isJSONObject reports whether body is a well-formed JSON object.
//
// Validity alone is not enough: 42, "text", [1,2] and null are all valid JSON
// documents and none of them is a configuration. Decoding into a map rejects the
// first three; null decodes into a nil map without error, so it is excluded
// explicitly.
func isJSONObject(body []byte) bool {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	return obj != nil
}

// writeJSONBytes writes an already-encoded JSON document as the response body.
//
// The sibling of writeJSON, for a body this server is forwarding rather than
// producing. Passing these bytes to writeJSON would not do the same thing: a
// []byte encodes as a base64 string, so the caller would receive a quoted blob
// instead of the document.
func writeJSONBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// fetchIdentityConfig GETs the configuration document, returning the body and
// status. The body is read even for an error status so a caller can report what
// was served.
//
// The read is capped as the card fetch is: this server is asking a process it
// does not control. A configuration is a few kilobytes, so a megabyte is
// generous; one that reaches the cap arrives truncated and is reported as
// unreadable.
func fetchIdentityConfig(ctx context.Context, configURL string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, identityConfigTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, configURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}
