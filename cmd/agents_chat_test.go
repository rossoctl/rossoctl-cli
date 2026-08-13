package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// newChatBackend serves the pieces `agents chat` needs from the platform API:
// the namespace list set-context validates against, and the agent card whose URL
// the command reads when --address is not given.
//
// cardURL is written into the card's "url" field, so a test can point the card at
// a real A2A agent and prove the address actually came from there. An empty
// cardURL yields a card with no URL at all, which is the case --address exists
// for.
//
// It also records every path it was asked for, so a test can assert the card was
// fetched — or, just as importantly, that it was not.
func newChatBackend(t *testing.T, cardURL string) (*httptest.Server, *[]string) {
	t.Helper()

	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces":
			_, _ = w.Write([]byte(`{"namespaces":["team1","team2"]}`))
		case "/api/v1/chat/team1/orders/agent-card", "/api/v1/chat/team2/orders/agent-card":
			fmt.Fprintf(w, `{"name":"Orders Agent","version":"1","url":%q}`, cardURL)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &paths
}

// TestAgentsChatUsesCardURL is the central case: with no --address, the agent is
// reached at the URL its card advertises. The card is pointed at a real A2A
// agent, so a wrong address could not produce this output.
func TestAgentsChatUsesCardURL(t *testing.T) {
	isolateHome(t)
	agent, _ := newEchoServer(t)
	backend, paths := newChatBackend(t, agent.URL)
	setupAgentGetContext(t, backend)

	out, err := execute(t, "agents", "chat", "orders", "--message", "hello there")
	if err != nil {
		t.Fatalf("agents chat: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "echo: hello there") {
		t.Errorf("output missing the echoed message:\n%s", out)
	}
	// The address must have come from the card, not from anywhere else.
	if !slices.Contains(*paths, "/api/v1/chat/team1/orders/agent-card") {
		t.Errorf("card was not fetched; requested paths = %v", *paths)
	}
}

// TestAgentsChatAddressSkipsCardLookup verifies --address is used directly and
// the card is not fetched at all. That is what makes the flag useful for an agent
// whose card is unavailable: a lookup that still happened could still fail.
func TestAgentsChatAddressSkipsCardLookup(t *testing.T) {
	isolateHome(t)
	agent, _ := newEchoServer(t)
	// The backend's card points somewhere unroutable, so a command that consulted
	// it would fail rather than quietly pass.
	backend, paths := newChatBackend(t, "http://192.0.2.1:9/")
	setupAgentGetContext(t, backend)

	out, err := execute(t, "agents", "chat", "orders",
		"--address", agent.URL, "--message", "direct")
	if err != nil {
		t.Fatalf("agents chat --address: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(out, "echo: direct") {
		t.Errorf("output missing the echoed message:\n%s", out)
	}
	if slices.Contains(*paths, "/api/v1/chat/team1/orders/agent-card") {
		t.Errorf("--address should skip the card lookup; requested paths = %v", *paths)
	}
}

// TestAgentsChatNamespaceOverride verifies the card is looked up in the
// namespace --namespace names, since that is what decides which agent "orders"
// means.
func TestAgentsChatNamespaceOverride(t *testing.T) {
	isolateHome(t)
	agent, _ := newEchoServer(t)
	backend, paths := newChatBackend(t, agent.URL)
	setupAgentGetContext(t, backend)

	if _, err := execute(t, "agents", "--namespace", "team2", "chat", "orders",
		"--message", "hi"); err != nil {
		t.Fatalf("agents chat: %v", err)
	}
	if !slices.Contains(*paths, "/api/v1/chat/team2/orders/agent-card") {
		t.Errorf("card not fetched from team2; requested paths = %v", *paths)
	}
}

// TestAgentsChatRequiresMessage verifies the missing flag is reported as such.
// It must fail before any request: there is nothing to send, so contacting the
// backend for a card would be wasted work whose own failure could mask the real
// problem.
func TestAgentsChatRequiresMessage(t *testing.T) {
	isolateHome(t)
	agent, _ := newEchoServer(t)
	backend, paths := newChatBackend(t, agent.URL)
	setupAgentGetContext(t, backend)

	out, err := execute(t, "agents", "chat", "orders")
	if err == nil {
		t.Fatalf("agents chat without --message should fail, got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--message") {
		t.Errorf("error = %q, want it to name --message", err)
	}
	if slices.Contains(*paths, "/api/v1/chat/team1/orders/agent-card") {
		t.Errorf("no request should be made without --message; paths = %v", *paths)
	}
}

// TestAgentsChatRequiresName verifies the agent name is required, so the command
// cannot run against an unnamed agent.
func TestAgentsChatRequiresName(t *testing.T) {
	isolateHome(t)
	if out, err := execute(t, "agents", "chat", "--message", "hi"); err == nil {
		t.Fatalf("agents chat without a name should fail, got:\n%s", out)
	}
}

// TestAgentsChatCardWithoutURL verifies a card carrying no URL is reported as
// such and names the way past it. The field is nominally required but comes from
// the agent, and `agents card` already renders it as "N/A" when absent, so this
// is reachable; passing "" on would fail later about a malformed endpoint
// instead.
func TestAgentsChatCardWithoutURL(t *testing.T) {
	isolateHome(t)
	backend, _ := newChatBackend(t, "")
	setupAgentGetContext(t, backend)

	out, err := execute(t, "agents", "chat", "orders", "--message", "hi")
	if err == nil {
		t.Fatalf("a card with no URL should fail, got:\n%s", out)
	}
	for _, want := range []string{"no URL", "--address"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// TestAgentsChatCardFetchFailureSurfaces verifies a backend that cannot produce a
// card fails the command rather than falling back to some other address. The 502
// is what the backend answers for an agent that is not running, which is the
// usual reason there is no card.
func TestAgentsChatCardFetchFailureSurfaces(t *testing.T) {
	isolateHome(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/namespaces" {
			_, _ = w.Write([]byte(`{"namespaces":["team1"]}`))
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"detail":"agent is not running"}`))
	}))
	t.Cleanup(srv.Close)
	setupAgentGetContext(t, srv)

	if out, err := execute(t, "agents", "chat", "orders", "--message", "hi"); err == nil {
		t.Fatalf("agents chat should fail when the card cannot be fetched, got:\n%s", out)
	}
}

// TestAgentsChatRejectsUnknownTransport verifies --transport is validated the way
// `a2a send` validates it, rather than being passed through to fail later with a
// less obvious message.
func TestAgentsChatRejectsUnknownTransport(t *testing.T) {
	isolateHome(t)
	agent, _ := newEchoServer(t)
	backend, _ := newChatBackend(t, agent.URL)
	setupAgentGetContext(t, backend)

	out, err := execute(t, "agents", "chat", "orders",
		"--message", "hi", "--transport", "carrier-pigeon")
	if err == nil {
		t.Fatalf("an unknown --transport should fail, got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "carrier-pigeon") {
		t.Errorf("error = %q, want it to name the rejected transport", err)
	}
}

// TestAgentsChatAcceptsJSONRPCSpellings verifies the friendly --transport
// spellings `a2a send` accepts work here too, over a real JSON-RPC agent.
func TestAgentsChatAcceptsJSONRPCSpellings(t *testing.T) {
	for _, transport := range []string{"jsonrpc", "json-rpc", "JSONRPC"} {
		t.Run(transport, func(t *testing.T) {
			isolateHome(t)
			agent, _ := newEchoServer(t)
			backend, _ := newChatBackend(t, agent.URL)
			setupAgentGetContext(t, backend)

			out, err := execute(t, "agents", "chat", "orders",
				"--message", "hi", "--transport", transport)
			if err != nil {
				t.Fatalf("--transport %s: %v\noutput:\n%s", transport, err, out)
			}
			if !strings.Contains(out, "echo: hi") {
				t.Errorf("output missing the echoed message:\n%s", out)
			}
		})
	}
}

// TestAgentsChatWithAuthorizationSendsHeader verifies --with-authorization puts
// the context's token on the wire to the agent, and that omitting it sends no
// Authorization header at all.
func TestAgentsChatWithAuthorizationSendsHeader(t *testing.T) {
	isolateHome(t)
	agent, exec := newEchoServer(t)
	backend, _ := newChatBackend(t, agent.URL)
	setupAgentGetContext(t, backend)

	// resolveServer reads the token from the config, so it has to be stored on
	// the context rather than injected.
	if _, err := execute(t, "login", "--token", "s3cr3t"); err != nil {
		t.Fatalf("login: %v", err)
	}

	if _, err := execute(t, "agents", "chat", "orders",
		"--message", "hi", "--with-authorization"); err != nil {
		t.Fatalf("agents chat --with-authorization: %v", err)
	}
	if got, want := exec.authHeader, "Bearer s3cr3t"; got != want {
		t.Errorf("Authorization header = %q, want %q", got, want)
	}

	exec.authHeader = ""
	if _, err := execute(t, "agents", "chat", "orders", "--message", "hi"); err != nil {
		t.Fatalf("agents chat: %v", err)
	}
	if exec.authHeader != "" {
		t.Errorf("Authorization header = %q, want it unset without --with-authorization", exec.authHeader)
	}
}

// TestAgentsChatWithAuthorizationNoToken verifies the flag fails loudly when the
// context has no token, rather than sending an unauthenticated request that the
// agent would reject for a reason the user cannot see.
func TestAgentsChatWithAuthorizationNoToken(t *testing.T) {
	isolateHome(t)
	agent, _ := newEchoServer(t)
	backend, _ := newChatBackend(t, agent.URL)
	setupAgentGetContext(t, backend)

	out, err := execute(t, "agents", "chat", "orders",
		"--message", "hi", "--with-authorization")
	if err == nil {
		t.Fatalf("--with-authorization without a token should fail, got:\n%s", out)
	}
	if !strings.Contains(err.Error(), "login") {
		t.Errorf("error = %q, want it to name `rossoctl login`", err)
	}
}

// TestAgentsChatStreamsArtifacts verifies chat renders the same event kinds
// `a2a send` does, including an artifact carrying a structured error. Both
// commands share one printer, so this pins that they really do.
func TestAgentsChatStreamsArtifacts(t *testing.T) {
	isolateHome(t)
	agent := newArtifactServer(t)
	backend, _ := newChatBackend(t, agent.URL)
	setupAgentGetContext(t, backend)

	out, err := execute(t, "agents", "chat", "orders", "--message", "go")
	if err != nil {
		t.Fatalf("agents chat: %v\noutput:\n%s", err, out)
	}
	for _, want := range []string{"crash-report", "stack overflow", `"code"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestAgentsChatVerboseLogsBothCalls verifies --verbose covers both requests the
// command makes: the card fetch against the platform API and the message to the
// agent. They go through different clients, so covering one proves nothing about
// the other.
func TestAgentsChatVerboseLogsBothCalls(t *testing.T) {
	isolateHome(t)
	agent, _ := newEchoServer(t)
	backend, _ := newChatBackend(t, agent.URL)
	setupAgentGetContext(t, backend)

	stdout, stderr, err := executeSplit(t, "agents", "chat", "orders", "--message", "hi", "--verbose")
	if err != nil {
		t.Fatalf("agents chat -v: %v\nstderr:\n%s", err, stderr)
	}
	// The card fetch, logged by the platform API client.
	if !strings.Contains(stderr, "/chat/team1/orders/agent-card") {
		t.Errorf("stderr missing the card fetch:\n%s", stderr)
	}
	// The message itself, logged by the A2A transport.
	if !strings.Contains(stderr, "POST "+agent.URL) {
		t.Errorf("stderr missing the A2A request to %s:\n%s", agent.URL, stderr)
	}
	if !strings.Contains(stdout, "echo: hi") {
		t.Errorf("stdout missing the streamed events:\n%s", stdout)
	}
	if strings.Contains(stdout, "POST ") {
		t.Errorf("verbose logging leaked into stdout:\n%s", stdout)
	}
}

// TestAgentsChat401HintWithoutFlag verifies an agent that rejects an
// unauthenticated call is answered with the advice to add the flag.
func TestAgentsChat401HintWithoutFlag(t *testing.T) {
	isolateHome(t)
	agent := newRejectingAgent(t, http.StatusUnauthorized)
	backend, _ := newChatBackend(t, agent.URL)
	setupAgentGetContext(t, backend)

	_, stderr, err := executeSplit(t, "agents", "chat", "orders", "--message", "hi")
	if err == nil {
		t.Fatal("a 401 from the agent should fail the command")
	}
	if !strings.Contains(stderr, "--with-authorization") {
		t.Errorf("stderr should suggest --with-authorization:\n%s", stderr)
	}
	// The other branch's advice would be wrong here: no token was sent, so there
	// is nothing yet to inspect.
	if strings.Contains(stderr, "auth status") {
		t.Errorf("stderr should not suggest inspecting a token that was never sent:\n%s", stderr)
	}
}

// TestAgentsChat401HintWithFlag verifies a token that was sent and rejected
// points at the token itself rather than at the flag already given.
func TestAgentsChat401HintWithFlag(t *testing.T) {
	isolateHome(t)
	agent := newRejectingAgent(t, http.StatusUnauthorized)
	backend, _ := newChatBackend(t, agent.URL)
	setupAgentGetContext(t, backend)
	if _, err := execute(t, "login", "--token", "s3cr3t"); err != nil {
		t.Fatalf("login: %v", err)
	}

	_, stderr, err := executeSplit(t, "agents", "chat", "orders",
		"--message", "hi", "--with-authorization")
	if err == nil {
		t.Fatal("a 401 from the agent should fail the command")
	}
	for _, want := range []string{"auth status", "login"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr should mention %q:\n%s", want, stderr)
		}
	}
	// Suggesting the flag that was already given would be useless advice.
	if strings.Contains(stderr, "Retry with --with-authorization") {
		t.Errorf("stderr should not re-suggest a flag already given:\n%s", stderr)
	}
}

// unresolvableAddress is a URL whose host cannot resolve anywhere. RFC 2606
// reserves .invalid for exactly this, so the DNS failure is guaranteed rather
// than dependent on the network the test runs on — a made-up name under a real
// TLD can be answered by a wildcard or a captive resolver.
const unresolvableAddress = "http://orders.team1.rossoctl-nonexistent.invalid:8080/"

// TestAgentsChatUnresolvedCardURLHint is the central case for this hint: the card
// advertises a hostname that does not resolve from here, which is what happens
// when the URL is cluster-internal. The advice has to explain that chat bypasses
// the platform API — the backend was reachable enough to serve the card, so the
// bare DNS error reads as a puzzle.
func TestAgentsChatUnresolvedCardURLHint(t *testing.T) {
	isolateHome(t)
	backend, _ := newChatBackend(t, unresolvableAddress)
	setupAgentGetContext(t, backend)

	_, stderr, err := executeSplit(t, "agents", "chat", "orders", "--message", "hi")
	if err == nil {
		t.Fatal("an address that does not resolve should fail the command")
	}
	// The three things the user needs: that this is a direct A2A call, the flag
	// that fixes it, and an example naming this very agent.
	for _, want := range []string{
		"A2A directly",
		"agent-card",
		"--address http://<route>:<port>",
		"--address http://orders.team1.localtest.me:8080",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr should mention %q:\n%s", want, stderr)
		}
	}
}

// TestAgentsChatUnresolvedHintUsesRealNamespace verifies the example is built
// from the namespace actually in effect, not a hardcoded one. An example naming
// the wrong namespace would be copied and would fail.
func TestAgentsChatUnresolvedHintUsesRealNamespace(t *testing.T) {
	isolateHome(t)
	backend, _ := newChatBackend(t, unresolvableAddress)
	setupAgentGetContext(t, backend)

	_, stderr, err := executeSplit(t, "agents", "--namespace", "team2", "chat", "orders",
		"--message", "hi")
	if err == nil {
		t.Fatal("an address that does not resolve should fail the command")
	}
	if !strings.Contains(stderr, "--address http://orders.team2.localtest.me:8080") {
		t.Errorf("the example should name the team2 namespace:\n%s", stderr)
	}
}

// TestAgentsChatUnresolvedAddressFlagHint verifies the hint also covers an
// address the user supplied. --address is the remedy the hint suggests, so a
// second unresolvable value passed to it is precisely when someone needs to be
// told the address, not the agent, is the problem.
func TestAgentsChatUnresolvedAddressFlagHint(t *testing.T) {
	isolateHome(t)
	backend, _ := newChatBackend(t, "")
	setupAgentGetContext(t, backend)

	_, stderr, err := executeSplit(t, "agents", "chat", "orders",
		"--address", unresolvableAddress, "--message", "hi")
	if err == nil {
		t.Fatal("an address that does not resolve should fail the command")
	}
	if !strings.Contains(stderr, "has to resolve from here") {
		t.Errorf("stderr should explain the address must resolve:\n%s", stderr)
	}
}

// TestAgentsChatNoUnresolvedHintOnReachableFailure verifies the hint is confined
// to a name that does not resolve. An agent that is reachable and answers an
// error has no address problem, and telling its user to go find a route would
// send them after the wrong thing.
func TestAgentsChatNoUnresolvedHintOnReachableFailure(t *testing.T) {
	isolateHome(t)
	agent := newRejectingAgent(t, http.StatusInternalServerError)
	backend, _ := newChatBackend(t, agent.URL)
	setupAgentGetContext(t, backend)

	_, stderr, err := executeSplit(t, "agents", "chat", "orders", "--message", "hi")
	if err == nil {
		t.Fatal("a 500 from the agent should fail the command")
	}
	if strings.Contains(stderr, "--address") {
		t.Errorf("a reachable agent's error should produce no address hint:\n%s", stderr)
	}
}

// TestAgentsChatNoUnresolvedHintOnRefusedConnection scopes the hint to DNS
// specifically. A host that resolves but refuses the connection is a different
// problem — the address is right and something is down — so the DNS advice must
// not fire. This is what matching *net.DNSError buys over matching the error
// text of a failed send.
func TestAgentsChatNoUnresolvedHintOnRefusedConnection(t *testing.T) {
	isolateHome(t)
	// A closed port on loopback: resolves fine, refuses immediately.
	backend, _ := newChatBackend(t, "http://127.0.0.1:1/")
	setupAgentGetContext(t, backend)

	_, stderr, err := executeSplit(t, "agents", "chat", "orders", "--message", "hi")
	if err == nil {
		t.Fatal("a refused connection should fail the command")
	}
	if strings.Contains(stderr, "Hint:") {
		t.Errorf("a refused connection is not a DNS failure; no hint expected:\n%s", stderr)
	}
}

// TestA2ASendNoUnresolvedHint verifies `a2a send` stays silent on the same
// failure. Its address came from the user's own --address, so there is nothing to
// reveal and no name to build an example from; the hint belongs to the command
// that resolved the address on the user's behalf.
func TestA2ASendNoUnresolvedHint(t *testing.T) {
	isolateHome(t)

	_, stderr, err := executeSplit(t, "a2a", "send",
		"--address", unresolvableAddress, "--message", "hi")
	if err == nil {
		t.Fatal("an address that does not resolve should fail the command")
	}
	if strings.Contains(stderr, "Hint:") {
		t.Errorf("a2a send should add no hint to its own --address:\n%s", stderr)
	}
}

// TestUnresolvedAgentAddressHintWithoutNamespace verifies an unresolvable
// namespace degrades to advice without an example, rather than to one naming an
// empty namespace. `--address http://orders..localtest.me:8080` would be a
// malformed URL offered as the fix.
func TestUnresolvedAgentAddressHintWithoutNamespace(t *testing.T) {
	hint := unresolvedAgentAddressHint("", "orders")
	if hint == "" {
		t.Fatal("the advice should survive a missing namespace")
	}
	if strings.Contains(hint, "For example") {
		t.Errorf("no example should be offered without a namespace:\n%s", hint)
	}
	if !strings.Contains(hint, "--address http://<route>:<port>") {
		t.Errorf("the generic form should still be suggested:\n%s", hint)
	}
}

// TestAgentsChatNoHintOnOtherFailures verifies the hint is confined to 401. A 403
// and a 500 are not credential problems, and advice that does not apply is worse
// than none.
func TestAgentsChatNoHintOnOtherFailures(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			isolateHome(t)
			agent := newRejectingAgent(t, status)
			backend, _ := newChatBackend(t, agent.URL)
			setupAgentGetContext(t, backend)

			_, stderr, err := executeSplit(t, "agents", "chat", "orders", "--message", "hi")
			if err == nil {
				t.Fatalf("a %d from the agent should fail the command", status)
			}
			if strings.Contains(stderr, "Hint:") {
				t.Errorf("a %d should produce no hint:\n%s", status, stderr)
			}
		})
	}
}
