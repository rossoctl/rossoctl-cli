package serve

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rossoctl/rossoctl-cli/internal/instances"
)

// stubConfigFetcher replaces the configuration fetch with a canned answer, so
// these tests do not need a live admin listener. The returned pointer holds the
// URL the handler asked for, which is what several of these tests assert on.
func stubConfigFetcher(t *testing.T, body string, status int, err error) *string {
	t.Helper()
	saved := configFetcher
	var requested string
	configFetcher = func(_ context.Context, configURL string) ([]byte, int, error) {
		requested = configURL
		return []byte(body), status, err
	}
	t.Cleanup(func() { configFetcher = saved })
	return &requested
}

// adminInstances are an a2a instance with an admin listener, one without, and an
// mcp instance.
//
// Local rather than an extension of mixedInstances: no fixture there carries an
// AdminAddr, and that one is asserted against by tests of the listing and detail
// endpoints which a new address would perturb.
//
// The instance with no AdminAddr is the ordinary case in practice, not a corner:
// `authbridge exec` starts an admin server only when hosting in a container.
func adminInstances() []instances.Instance {
	return []instances.Instance{
		{
			ID:              "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			Name:            "bright-mesa-0001",
			Namespace:       "recorded1",
			InboundProtocol: instances.ProtocolA2A,
			InboundAddr:     "127.0.0.1:8080",
			AdminAddr:       "127.0.0.1:9093",
			CommandLine:     []string{"python", "agent.py"},
		},
		{
			ID:              "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
			Name:            "quiet-basin-0002",
			Namespace:       "recorded1",
			InboundProtocol: instances.ProtocolA2A,
			InboundAddr:     "127.0.0.1:8082",
			CommandLine:     []string{"python", "other.py"},
		},
		{
			ID:              "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
			Name:            "warm-delta-0003",
			Namespace:       "recorded1",
			InboundProtocol: instances.ProtocolMCP,
			InboundAddr:     "127.0.0.1:8081",
			AdminAddr:       "127.0.0.1:9094",
			CommandLine:     []string{"npx", "mcp-server"},
		},
	}
}

// configPath is the endpoint under test for the instance that has an admin
// listener.
const configPath = "/api/v1/agents/recorded1/bright-mesa-0001/identity-config"

// authbridgeConfig is a configuration as AuthBridge's /config endpoint serves
// one: the blocks this server's client models (mode, pipeline) alongside ones it
// does not (listener, session, stats), and a value already replaced by the
// upstream's redaction.
const authbridgeConfig = `{"mode":"proxy-sidecar","listener":{"reverse_proxy_addr":":8080"},` +
	`"pipeline":{"inbound":{"plugins":[{"name":"jwt-validation"},` +
	`{"name":"opa","config":{"policy_url":"http://opa:8181","api_key":"[REDACTED]"}}]},` +
	`"outbound":{"plugins":[{"name":"token-exchange"}]}},` +
	`"session":{"enabled":true},"stats":{"address":":9093"}}`

// getRawConfig requests the endpoint and returns the raw body and content type,
// failing on a non-200. Raw rather than decoded because the point of several of
// these assertions is the bytes themselves.
func getRawConfig(t *testing.T, ts *httptest.Server, path string) (string, string) {
	t.Helper()
	res, err := ts.Client().Get(ts.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200; body: %s", path, res.StatusCode, body)
	}
	return string(body), res.Header.Get("Content-Type")
}

// TestIdentityConfigFetchesFromTheAdminAddress verifies the fetch targets the
// instance's admin address and the /config path.
//
// This is the whole reason the endpoint consults the record. An implementation
// copied from the agent-card handler would reach for InboundAddr, which is a
// different listener on a different port that serves no configuration.
func TestIdentityConfigFetchesFromTheAdminAddress(t *testing.T) {
	stubGetter(t, adminInstances())
	requested := stubConfigFetcher(t, authbridgeConfig, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	if _, _ = getRawConfig(t, ts, configPath); *requested != "http://127.0.0.1:9093/config" {
		t.Errorf("fetched %q, want http://127.0.0.1:9093/config", *requested)
	}
}

// TestIdentityConfigForwardsBodyByteForByte verifies the response body is exactly
// what the admin endpoint served.
//
// Byte equality rather than a decoded comparison: a re-marshal would reorder keys
// and re-indent while still decoding equal, and passing the bytes to writeJSON
// would base64 them into a quoted string. Both pass a shape assertion and fail
// this one.
func TestIdentityConfigForwardsBodyByteForByte(t *testing.T) {
	stubGetter(t, adminInstances())
	stubConfigFetcher(t, authbridgeConfig, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	body, _ := getRawConfig(t, ts, configPath)
	if body != authbridgeConfig {
		t.Errorf("body was not forwarded unchanged:\n got: %s\nwant: %s", body, authbridgeConfig)
	}
}

// TestIdentityConfigPreservesRedactionAndUnknownBlocks verifies the redacted
// value survives and that blocks this server does not model are still present.
//
// Both are consequences of forwarding rather than decoding, and both matter:
// redaction is applied upstream, so a response that re-marshaled through a
// partial struct could drop the blocks a reader came for and would no longer be
// the document whose secrets were already replaced.
func TestIdentityConfigPreservesRedactionAndUnknownBlocks(t *testing.T) {
	stubGetter(t, adminInstances())
	stubConfigFetcher(t, authbridgeConfig, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	body, _ := getRawConfig(t, ts, configPath)
	if !strings.Contains(body, `"api_key":"[REDACTED]"`) {
		t.Errorf("the upstream's redacted value should survive verbatim:\n%s", body)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{"mode", "pipeline", "listener", "session", "stats"} {
		if _, ok := got[key]; !ok {
			t.Errorf("response dropped the %q block:\n%s", key, body)
		}
	}
}

// TestIdentityConfigContentTypeIsJSON verifies the JSON content type is set. The
// raw-bytes writer does not go through writeJSON, so the header is its own
// responsibility.
func TestIdentityConfigContentTypeIsJSON(t *testing.T) {
	stubGetter(t, adminInstances())
	stubConfigFetcher(t, authbridgeConfig, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	if _, ct := getRawConfig(t, ts, configPath); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

// TestIdentityConfigWithoutAdminAddressIsUnavailable verifies an instance with no
// admin listener reports 503 naming the instance, and does not describe it as
// unreachable.
//
// The distinction is the point: this instance is healthy and was simply started
// without an admin server, which is the common case for `authbridge exec`.
// Reporting it the way the connect failure is reported would send a reader
// looking for a down agent.
func TestIdentityConfigWithoutAdminAddressIsUnavailable(t *testing.T) {
	stubGetter(t, adminInstances())
	requested := stubConfigFetcher(t, authbridgeConfig, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	status, detail := cardStatus(t, ts, "/api/v1/agents/recorded1/quiet-basin-0002/identity-config")
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}
	if !strings.Contains(detail, "quiet-basin-0002") {
		t.Errorf("detail should name the instance, got %q", detail)
	}
	if !strings.Contains(detail, "admin listener") {
		t.Errorf("detail should say what is missing, got %q", detail)
	}
	// Nothing was asked, so nothing can be reported as unreachable.
	if *requested != "" {
		t.Errorf("no fetch should be attempted, got %q", *requested)
	}
	if strings.Contains(detail, "connect") {
		t.Errorf("a healthy instance should not be reported as unreachable: %q", detail)
	}
}

// TestIdentityConfigUnreachableAdminIsUnavailable verifies a failed fetch reports
// 503 naming the address, distinguishing it from the branch above.
func TestIdentityConfigUnreachableAdminIsUnavailable(t *testing.T) {
	stubGetter(t, adminInstances())
	stubConfigFetcher(t, "", 0, errors.New("connection refused"))
	ts := newTestServer(t, "/api/v1")

	status, detail := cardStatus(t, ts, configPath)
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", status)
	}
	if !strings.Contains(detail, "127.0.0.1:9093") {
		t.Errorf("detail should name the admin address, got %q", detail)
	}
	if !strings.Contains(detail, "connection refused") {
		t.Errorf("detail should report the failure, got %q", detail)
	}
}

// TestIdentityConfigPassesThroughAdminStatus verifies a non-2xx from the admin
// endpoint is forwarded with its own status.
//
// Collapsing these to one code would hide which hop refused — a 404 from the
// admin endpoint means the listener is not an AuthBridge one, which is a
// different finding from this server failing.
func TestIdentityConfigPassesThroughAdminStatus(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			stubGetter(t, adminInstances())
			stubConfigFetcher(t, `{"error":"nope"}`, status, nil)
			ts := newTestServer(t, "/api/v1")

			got, detail := cardStatus(t, ts, configPath)
			if got != status {
				t.Errorf("status = %d, want the admin endpoint's %d", got, status)
			}
			if !strings.Contains(detail, "127.0.0.1:9093") {
				t.Errorf("detail should name the admin address, got %q", detail)
			}
			// A forwarded 500 must not be mistakable for this route being a
			// placeholder.
			if detail == unimplementedMessage {
				t.Errorf("detail should describe the upstream failure, not read as UNIMPLEMENTED")
			}
		})
	}
}

// TestIdentityConfigNonJSONBodyIsBadGateway verifies a 2xx body that is not JSON
// is reported as a bad answer rather than forwarded.
//
// Without the check this would answer 200 with an HTML page under a JSON content
// type, which a client reports as a decode failure of this server rather than of
// the admin endpoint.
func TestIdentityConfigNonJSONBodyIsBadGateway(t *testing.T) {
	stubGetter(t, adminInstances())
	stubConfigFetcher(t, "<html>not a config</html>", http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	res, err := ts.Client().Get(ts.URL + configPath)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)

	if res.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", res.StatusCode)
	}
	if strings.Contains(string(body), "<html>") {
		t.Errorf("the unreadable body should not be forwarded:\n%s", body)
	}
}

// TestIdentityConfigNonObjectBodyIsBadGateway verifies a body that is valid JSON
// but not an object is rejected.
//
// A validity check alone passes every one of these, and null in particular
// decodes into a map without error — so it would be forwarded as if it were a
// configuration.
func TestIdentityConfigNonObjectBodyIsBadGateway(t *testing.T) {
	for _, body := range []string{`42`, `[1,2]`, `"a string"`, `null`, `true`} {
		t.Run(body, func(t *testing.T) {
			stubGetter(t, adminInstances())
			stubConfigFetcher(t, body, http.StatusOK, nil)
			ts := newTestServer(t, "/api/v1")

			status, detail := cardStatus(t, ts, configPath)
			if status != http.StatusBadGateway {
				t.Errorf("body %s: status = %d, want 502", body, status)
			}
			if !strings.Contains(detail, "unreadable") {
				t.Errorf("detail = %q, want it to report an unreadable configuration", detail)
			}
		})
	}
}

// TestIdentityConfigTruncatedJSONIsBadGateway verifies a body cut off mid-document
// is reported rather than forwarded. This is what makes the read cap safe and not
// merely bounded: a configuration that hits it arrives truncated.
func TestIdentityConfigTruncatedJSONIsBadGateway(t *testing.T) {
	stubGetter(t, adminInstances())
	stubConfigFetcher(t, `{"mode":"proxy-sidecar","pipe`, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	if status, _ := cardStatus(t, ts, configPath); status != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", status)
	}
}

// TestIdentityConfigMissingInstanceIsNotFound verifies an unknown agent 404s, as
// the sibling endpoints do.
func TestIdentityConfigMissingInstanceIsNotFound(t *testing.T) {
	stubGetter(t, adminInstances())
	stubConfigFetcher(t, authbridgeConfig, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	if status, _ := cardStatus(t, ts, "/api/v1/agents/recorded1/nope-0000/identity-config"); status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// TestIdentityConfigMCPInstanceIsNotFound verifies an mcp instance is not an agent
// on this endpoint, even though it has an admin address of its own.
func TestIdentityConfigMCPInstanceIsNotFound(t *testing.T) {
	stubGetter(t, adminInstances())
	requested := stubConfigFetcher(t, authbridgeConfig, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	status, _ := cardStatus(t, ts, "/api/v1/agents/recorded1/warm-delta-0003/identity-config")
	if status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
	if *requested != "" {
		t.Errorf("no fetch should be attempted for an mcp instance, got %q", *requested)
	}
}

// TestIdentityConfigGetterErrorIsInternal verifies a failure to read the record
// is a 500, not a 404: an unreadable record is not an absent instance.
func TestIdentityConfigGetterErrorIsInternal(t *testing.T) {
	stubGetterErr(t, errors.New("permission denied"))
	stubConfigFetcher(t, authbridgeConfig, http.StatusOK, nil)
	ts := newTestServer(t, "/api/v1")

	if status, _ := cardStatus(t, ts, configPath); status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
}

// TestWriteJSONBytesDoesNotEncode verifies the raw writer emits its bytes
// literally, with the status and content type set.
//
// A unit test because it guards the exact hazard the forwarding path exists to
// avoid: routed through an encoder, a []byte body becomes a base64 string.
func TestWriteJSONBytesDoesNotEncode(t *testing.T) {
	rec := httptest.NewRecorder()
	const body = `{"mode":"proxy-sidecar"}`

	writeJSONBytes(rec, http.StatusOK, []byte(body))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got := rec.Body.String(); got != body {
		t.Errorf("body = %q, want it written literally as %q", got, body)
	}
}
